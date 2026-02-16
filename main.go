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
)

// --- Constants ---
const (
	StatusNotStarted = 0
	StatusInProgress = 1
	StatusCompleted  = 2
)

// --- Data Structures ---
type Project struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      interface{} `json:"status"`
}

type Tag struct {
	Name string `json:"name"`
}

type Task struct {
	ID        int    `json:"id,omitempty"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	Status    int    `json:"status"` // Changed to int
	Priority  string `json:"priority"`
	ProjectID int    `json:"project_id"`
	DueDate   string `json:"due_date,omitempty"`
	Tags      []Tag  `json:"tags,omitempty"`
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
		log.Println("⚠️  DRY RUN MODE ENABLED: No changes will be made to Tududi ⚠️")
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
	
	// 1. Projects
	projects := fetchTududiProjects()
	projectMap := make(map[string]int) // Name -> ID
	for _, p := range projects {
		projectMap[normalizeName(p.Name)] = p.ID
	}
	log.Printf("Loaded %d existing projects", len(projects))

	// 2. Tasks
	// We build a map of "ProjectID + TaskName" -> Task to detect duplicates
	// Since Tududi likely ignores our custom UID, we must rely on Name + Project
	existingTasks := fetchTududiTasks()
	taskDedupMap := make(map[string]Task)

	for _, t := range existingTasks {
		// Key: "ProjectID|TaskName" (normalized)
		key := fmt.Sprintf("%d|%s", t.ProjectID, normalizeName(t.Name))
		taskDedupMap[key] = t
	}

	// Mock ID for dry run new projects
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

		// Determine Status (Integer)
		targetStatus := StatusNotStarted
		if issue.GetState() == "closed" {
			targetStatus = StatusCompleted
		}

		// --- RESOLVE PROJECT ---
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
					log.Printf("Skipping issue %d (Project creation failed)", issue.GetNumber())
					continue
				}
			}
		}

		// --- DEDUPLICATION CHECK ---
		// Key: "ProjectID|TaskName"
		dedupKey := fmt.Sprintf("%d|%s", projID, normalizeName(issue.GetTitle()))
		
		if task, found := taskDedupMap[dedupKey]; found {
			// Task Exists - Check Status Drift
			if task.Status != targetStatus {
				// Only update if statuses actually differ
				// Note: User defines: 0=not_started, 1=in_progress, 2=completed
				
				// If GH is closed (2), but local is pending (0) or progress (1) -> Update to 2
				// If GH is open (0), but local is completed (2) -> Reopen to 0
				
				shouldUpdate := false
				if targetStatus == StatusCompleted && task.Status != StatusCompleted {
					shouldUpdate = true
				} else if targetStatus == StatusNotStarted && task.Status == StatusCompleted {
					shouldUpdate = true
				}

				if shouldUpdate {
					log.Printf("[UPDATE] Task '%s' status change: %d -> %d", task.Name, task.Status, targetStatus)
					updateTaskStatus(task.ID, targetStatus)
				}
			}
			continue // Task already exists, move to next
		}

		// --- CREATE NEW TASK ---
		createTududiTask(issue, projID, repoName, targetStatus)
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
	type TaskResponse struct {
		Tasks []Task `json:"tasks"`
	}
	var resp TaskResponse
	
	// Ensure we fetch ALL tasks to perform accurate deduplication
	err := makeRequest("GET", "/tasks?type=all", nil, &resp)
	if err == nil { return resp.Tasks }

	var arrayResp []Task
	makeRequest("GET", "/tasks?type=all", nil, &arrayResp)
	return arrayResp
}

func createTududiProject(name, description, status string) int {
	if description == "" {
		description = fmt.Sprintf("Imported GitHub Repository: %s", name)
	}
	// Tududi might accept int or string for status on creation. 
	// If it fails with string, user might need to map "planned" -> int.
	// For now, keeping string as per previous logs saying projects were created fine.
	payload := map[string]interface{}{
		"name":        name,
		"status":      status, 
		"description": description,
		"priority":    "medium",
	}
	var result Project
	err := makeRequest("POST", "/project", payload, &result)
	if err != nil {
		log.Printf("Failed to create project: %v", err)
		return 0
	}
	return result.ID
}

func createTududiTask(issue *github.Issue, projectID int, repoName string, status int) {
	if dryRun {
		log.Printf("[DRY RUN] Would create Task: '%s' [Status: %d] in ProjectID %d", issue.GetTitle(), status, projectID)
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
	
	tags := []Tag{
		{Name: repoName},
		{Name: "github"},
	}

	task := Task{
		Name:      issue.GetTitle(),
		Note:      note,
		Status:    status, // Sending INT here (0, 1, 2)
		Priority:  priority,
		ProjectID: projectID,
		Tags:      tags,
	}

	if issue.Milestone != nil && issue.Milestone.DueOn != nil {
		task.DueDate = issue.Milestone.DueOn.Format(time.RFC3339)
	}

	err := makeRequest("POST", "/task", task, nil)
	if err == nil {
		log.Printf("Created Task: %s [Status: %d]", task.Name, status)
	}
}

func updateTaskStatus(taskID int, status int) {
	if dryRun {
		log.Printf("[DRY RUN] Would update Task %d status to %d", taskID, status)
		return
	}

	// Payload uses INT for status
	payload := map[string]int{
		"status": status,
	}
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

	for k, v := range getHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 404 || method != "GET" {
			log.Printf("API Error (%d) on %s: %s", resp.StatusCode, endpoint, string(b))
		}
		return fmt.Errorf("API Error %d", resp.StatusCode)
	}

	if target != nil {
		return json.NewDecoder(resp.Body).Decode(target)
	}
	return nil
}