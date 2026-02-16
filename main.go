package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v60/github"
	"golang.org/x/oauth2"
)

// --- Configuration ---
var (
	githubToken  = os.Getenv("GITHUB_TOKEN")
	tududiURL    = strings.TrimRight(os.Getenv("TUDUDI_URL"), "/")
	tududiAPIKey = os.Getenv("TUDUDI_API_KEY")
	syncInterval = os.Getenv("SYNC_INTERVAL")
	dryRun       = os.Getenv("DRY_RUN") == "true"
	debugMode    = os.Getenv("DEBUG") == "true" // NEW: Enable verbose logs
)

const (
	StatusNotStarted = 0
	StatusInProgress = 1
	StatusCompleted  = 2
)

// --- Data Structures ---
type Project struct {
	ID          int         `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Status      interface{} `json:"status"`
}

type Tag struct {
	Name string `json:"name"`
}

type Task struct {
	ID        int    `json:"id"` // Ensure this parses correctly
	Name      string `json:"name"`
	Status    int    `json:"status"`
	ProjectID int    `json:"project_id"`
	UID       string `json:"uid,omitempty"`
}

func main() {
	if githubToken == "" || tududiAPIKey == "" {
		log.Fatal("Missing GITHUB_TOKEN or TUDUDI_API_KEY environment variables.")
	}
	if tududiURL == "" {
		tududiURL = "http://localhost:3002/api/v1"
	}

	interval, err := strconv.Atoi(syncInterval)
	if err != nil || interval < 10 {
		interval = 300
	}

	if dryRun {
		log.Println("⚠️  DRY RUN MODE ENABLED ⚠️")
	}
	if debugMode {
		log.Println("🐛 DEBUG MODE ENABLED 🐛")
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: githubToken})
	tc := oauth2.NewClient(ctx, ts)
	ghClient := github.NewClient(tc)

	log.Printf("Starting Sync Service. Interval: %d seconds", interval)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	runSync(ctx, ghClient)

	for range ticker.C {
		runSync(ctx, ghClient)
	}
}

func runSync(ctx context.Context, gh *github.Client) {
	log.Println("--- Starting Sync Cycle ---")

	user, _, err := gh.Users.Get(ctx, "")
	if err != nil {
		log.Printf("Error getting GitHub user: %v", err)
		return
	}
	myLogin := user.GetLogin()

	processedIDs := make(map[int64]bool)
	var issuesToSync []*github.Issue

	// 1. Fetch Issues
	opts := &github.SearchOptions{Sort: "updated", Order: "desc"}
	query := fmt.Sprintf("assignee:%s is:issue", myLogin)
	
	result, _, err := gh.Search.Issues(ctx, query, opts)
	if err != nil {
		log.Printf("Error searching issues: %v", err)
	} else {
		count := 0
		for _, issue := range result.Issues {
			if count >= 50 { break }
			if !processedIDs[issue.GetID()] {
				issuesToSync = append(issuesToSync, issue)
				processedIDs[issue.GetID()] = true
				count++
			}
		}
	}

	// 2. Fetch Repos
	repoOpts := &github.RepositoryListOptions{Type: "owner", ListOptions: github.ListOptions{PerPage: 100}}
	repos, _, err := gh.Repositories.List(ctx, "", repoOpts)
	if err != nil {
		log.Printf("Error listing repos: %v", err)
	} else {
		for _, repo := range repos {
			if repo.GetOwner().GetLogin() == myLogin {
				issueOpts := &github.IssueListByRepoOptions{
					State: "all", Sort: "updated", Direction: "desc",
					ListOptions: github.ListOptions{PerPage: 20},
				}
				repoIssues, _, err := gh.Issues.ListByRepo(ctx, myLogin, repo.GetName(), issueOpts)
				if err != nil {
					log.Printf("Error getting issues for %s: %v", repo.GetName(), err)
					continue
				}
				for _, issue := range repoIssues {
					if issue.IsPullRequest() { continue }
					if !processedIDs[issue.GetID()] {
						issuesToSync = append(issuesToSync, issue)
						processedIDs[issue.GetID()] = true
					}
				}
			}
		}
	}

	log.Printf("Processing %d GitHub issues...", len(issuesToSync))
	syncIssuesToTududi(issuesToSync)
}

func syncIssuesToTududi(issues []*github.Issue) {
	// --- FETCH DATA ---
	
	projects := fetchTududiProjects()
	projectMap := make(map[string]int)
	for _, p := range projects {
		projectMap[normalizeName(p.Name)] = p.ID
	}
	log.Printf("Loaded %d existing PROJECTS", len(projects))

	// Fetch Tasks and build deduplication map
	existingTasks := fetchTududiTasks()
	taskDedupMap := make(map[string]Task)

	for _, t := range existingTasks {
		// Key: "ProjectID|TaskName"
		key := fmt.Sprintf("%d|%s", t.ProjectID, normalizeName(t.Name))
		taskDedupMap[key] = t
	}
	log.Printf("Loaded %d existing TASKS for deduplication", len(existingTasks))

	mockProjectIDCounter := -1

	// --- PROCESS ISSUES ---
	for _, issue := range issues {
		repo := issue.GetRepository()
		
		var repoName, repoDesc string
		var isArchived bool
		
		if repo != nil {
			repoName = repo.GetName()
			repoDesc = repo.GetDescription()
			isArchived = repo.GetArchived()
		} else {
			if issue.RepositoryURL != nil {
				parts := strings.Split(*issue.RepositoryURL, "/")
				repoName = parts[len(parts)-1]
			}
			repoDesc = fmt.Sprintf("Imported GitHub Repository: %s", repoName)
		}

		targetStatus := StatusNotStarted
		if issue.GetState() == "closed" {
			targetStatus = StatusCompleted
		}

		// Resolve Project
		normRepoName := normalizeName(repoName)
		projID, exists := projectMap[normRepoName]
		
		if !exists {
			projectStatus := "planned"
			if isArchived {
				projectStatus = "done"
			}

			if dryRun {
				log.Printf("[DRY RUN] Would create project: '%s'", repoName)
				projectMap[normRepoName] = mockProjectIDCounter
				projID = mockProjectIDCounter
				mockProjectIDCounter--
			} else {
				log.Printf("Project '%s' not found. Creating...", repoName)
				newID := createTududiProject(repoName, repoDesc, projectStatus)
				if newID != 0 {
					projectMap[normRepoName] = newID
					projID = newID
				} else {
					continue
				}
			}
		}

		// Deduplication Check
		dedupKey := fmt.Sprintf("%d|%s", projID, normalizeName(issue.GetTitle()))
		
		if task, found := taskDedupMap[dedupKey]; found {
			// Found existing task - check status
			if debugMode {
				log.Printf("Dedup match: '%s' (ID: %d, Status: %d, Target: %d)", task.Name, task.ID, task.Status, targetStatus)
			}
			
			// If GitHub is Closed (2) and Task is Not Completed (0 or 1)
			if targetStatus == StatusCompleted && task.Status != StatusCompleted {
				log.Printf("[UPDATE] Task '%s' marked completed in GitHub.", task.Name)
				updateTaskStatus(task.ID, StatusCompleted)
			} else if targetStatus == StatusNotStarted && task.Status == StatusCompleted {
				log.Printf("[UPDATE] Task '%s' re-opened in GitHub.", task.Name)
				updateTaskStatus(task.ID, StatusNotStarted)
			}
			continue
		} else {
			if debugMode {
				log.Printf("No dedup match for key: [%s]", dedupKey)
			}
		}

		// Create New
		createTududiTask(issue, projID, repoName, targetStatus)
		
		// Add to local map to prevent duplication within the same run cycle
		taskDedupMap[dedupKey] = Task{Name: issue.GetTitle(), ProjectID: projID, Status: targetStatus}
	}
}

// --- HELPERS ---

func getHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + tududiAPIKey,
		"Content-Type":  "application/json",
	}
}

func normalizeName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	return strings.TrimSpace(name)
}

func fetchTududiProjects() []Project {
	type ProjectResponse struct {
		Projects []Project `json:"projects"`
	}
	var resp ProjectResponse
	err := makeRequest("GET", "/projects?status=all", nil, &resp)
	if err == nil && len(resp.Projects) > 0 { return resp.Projects }

	var projects []Project
	makeRequest("GET", "/projects?status=all", nil, &projects)
	return projects
}

func fetchTududiTasks() []Task {
	// Try fetching ALL tasks
	// We handle both Array and Object return types
	type TaskResponse struct {
		Tasks []Task `json:"tasks"`
	}
	var resp TaskResponse
	
	// 1. Try ?type=all
	err := makeRequest("GET", "/tasks?type=all", nil, &resp)
	if err == nil && len(resp.Tasks) > 0 { return resp.Tasks }

	// 2. Fallback: Array response
	var arrayResp []Task
	if makeRequest("GET", "/tasks?type=all", nil, &arrayResp) == nil && len(arrayResp) > 0 {
		return arrayResp
	}

	// 3. Last Resort: Try without filters (some APIs default to all if no filter)
	if makeRequest("GET", "/tasks", nil, &resp) == nil { return resp.Tasks }

	return []Task{}
}

func createTududiProject(name, description, status string) int {
	if description == "" { description = fmt.Sprintf("Imported GitHub Repository: %s", name) }
	
	payload := map[string]interface{}{
		"name": name, "status": status, "description": description, "priority": "medium",
	}
	var result Project
	err := makeRequest("POST", "/project", payload, &result)
	if err != nil { return 0 }
	return result.ID
}

func createTududiTask(issue *github.Issue, projectID int, repoName string, status int) {
	if dryRun {
		log.Printf("[DRY RUN] Would create Task: '%s' [Status: %d]", issue.GetTitle(), status)
		return
	}

	note := issue.GetBody()
	note += fmt.Sprintf("\n\n**GitHub Source**: [Issue #%d](%s)", issue.GetNumber(), issue.GetHTMLURL())

	priority := "medium"
	for _, label := range issue.Labels {
		lname := strings.ToLower(label.GetName())
		if strings.Contains(lname, "urgent") || strings.Contains(lname, "high") {
			priority = "high"
		}
	}
	
	tags := []Tag{{Name: repoName}, {Name: "github"}}

	task := map[string]interface{}{
		"name": issue.GetTitle(),
		"note": note,
		"status": status, // Sending INT
		"priority": priority,
		"project_id": projectID,
		"tags": tags,
	}

	if issue.Milestone != nil && issue.Milestone.DueOn != nil {
		task["due_date"] = issue.Milestone.DueOn.Format(time.RFC3339)
	}

	// Make request but ignore return body, we just check error
	err := makeRequest("POST", "/task", task, nil)
	if err == nil {
		log.Printf("Created Task: %s [Status: %d]", issue.GetTitle(), status)
	}
}

func updateTaskStatus(taskID int, status int) {
	if dryRun {
		log.Printf("[DRY RUN] Would update Task %d status to %d", taskID, status)
		return
	}
	payload := map[string]int{"status": status}
	endpoint := fmt.Sprintf("/task/%d", taskID)
	makeRequest("PATCH", endpoint, payload, nil)
}

func makeRequest(method, endpoint string, body interface{}, target interface{}) error {
	client := &http.Client{Timeout: 10 * time.Second}
	var bodyReader io.Reader
	if body != nil {
		jsonBytes, _ := json.Marshal(body)
		bodyReader = bytes.NewBuffer(jsonBytes)
	}
	req, err := http.NewRequest(method, tududiURL+endpoint, bodyReader)
	if err != nil { return err }
	for k, v := range getHeaders() { req.Header.Set(k, v) }

	resp, err := client.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()

	if debugMode {
		log.Printf("[DEBUG] %s %s -> %d", method, endpoint, resp.StatusCode)
	}

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		if debugMode { log.Printf("[DEBUG] Error Body: %s", string(b)) }
		if resp.StatusCode != 404 || method != "GET" {
			log.Printf("API Error (%d) on %s", resp.StatusCode, endpoint)
		}
		return fmt.Errorf("API Error %d", resp.StatusCode)
	}

	if target != nil {
		// Dump body to debug if needed, otherwise strict decode
		if debugMode {
			bodyBytes, _ := io.ReadAll(resp.Body)
			// log.Printf("[DEBUG] Response: %s", string(bodyBytes))
			return json.Unmarshal(bodyBytes, target)
		}
		return json.NewDecoder(resp.Body).Decode(target)
	}
	return nil
}