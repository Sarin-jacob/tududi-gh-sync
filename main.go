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

// --- Data Structures ---
type Project struct {
	ID          int         `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Status      interface{} `json:"status"`
	UID         string      `json:"uid,omitempty"`
}

type Tag struct {
	Name string `json:"name"`
}

type Task struct {
	ID        int         `json:"id,omitempty"`
	UID       string      `json:"uid,omitempty"`
	Name      string      `json:"name"`
	Note      string      `json:"note"`
	Status    interface{} `json:"status"`
	Priority  string      `json:"priority"`
	ProjectID int         `json:"project_id"`
	DueDate   string      `json:"due_date,omitempty"`
	Tags      []Tag       `json:"tags,omitempty"`
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
	existingTasks := fetchTududiTasks()
	existingTaskMap := make(map[string]Task)
	for _, t := range existingTasks {
		if t.UID != "" {
			existingTaskMap[t.UID] = t
		}
	}

	projects := fetchTududiProjects()
	projectMap := make(map[string]int)

	var projectNames []string
	for _, p := range projects {
		norm := normalizeName(p.Name)
		projectMap[norm] = p.ID
		projectNames = append(projectNames, fmt.Sprintf("%s", p.Name))
	}
	log.Printf("Loaded %d existing projects: %v", len(projects), projectNames)

	// Dry Run Mock ID Counter (starts negative to avoid collision with real IDs)
	mockProjectIDCounter := -1

	for _, issue := range issues {
		repo := issue.GetRepository()
		
		var repoID int64
		var repoName, repoDesc string
		var isArchived bool
		
		if repo != nil {
			repoID = repo.GetID()
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
		
		tududiUID := fmt.Sprintf("gh_%d_%d", repoID, issue.GetNumber())
		
		targetStatus := "pending"
		if issue.GetState() == "closed" {
			targetStatus = "completed"
		}

		// 1. UPDATE EXISTING
		if task, exists := existingTaskMap[tududiUID]; exists {
			ghIsClosed := (targetStatus == "completed")
			tududiStatus := fmt.Sprintf("%v", task.Status) 
			tududiIsDone := (tududiStatus == "completed" || tududiStatus == "2" || tududiStatus == "archived")

			if ghIsClosed && !tududiIsDone {
				log.Printf("[UPDATE] Task '%s' marked completed in GitHub.", task.Name)
				updateTaskStatus(task.ID, "completed")
			} else if !ghIsClosed && tududiIsDone {
				log.Printf("[UPDATE] Task '%s' re-opened in GitHub.", task.Name)
				updateTaskStatus(task.ID, "pending")
			}
			continue
		}

		// 2. CREATE NEW
		normRepoName := normalizeName(repoName)
		projID, exists := projectMap[normRepoName]
		
		if !exists {
			projectStatus := "planned"
			if isArchived {
				projectStatus = "done"
			}

			if dryRun {
				// Assign a mock ID so subsequent issues for this repo don't trigger "Would create" again
				log.Printf("[DRY RUN] Would create project: '%s' (Status: %s)", repoName, projectStatus)
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

		createTududiTask(issue, projID, tududiUID, repoName, targetStatus)
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
	if err == nil && len(resp.Projects) > 0 {
		return resp.Projects
	}

	var projects []Project
	if makeRequest("GET", "/projects?status=all", nil, &projects) == nil {
		return projects
	}
	
	log.Printf("Warning: Failed to fetch projects (Check API response format): %v", err)
	return []Project{}
}

func fetchTududiTasks() []Task {
	type TaskResponse struct {
		Tasks []Task `json:"tasks"`
	}
	var resp TaskResponse
	
	err := makeRequest("GET", "/tasks?type=all", nil, &resp)
	if err != nil {
		var arrayResp []Task
		if makeRequest("GET", "/tasks?type=all", nil, &arrayResp) == nil {
			return arrayResp
		}
	}
	return resp.Tasks
}

func createTududiProject(name, description, status string) int {
	if description == "" {
		description = fmt.Sprintf("Imported GitHub Repository: %s", name)
	}
	
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

func createTududiTask(issue *github.Issue, projectID int, uid string, repoName string, status string) {
	// If Dry Run, explicitly Log the Status we WOULD create
	if dryRun {
		log.Printf("[DRY RUN] Would create Task: '%s' [%s] in ProjectID %d", issue.GetTitle(), status, projectID)
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
		UID:       uid,
		Name:      issue.GetTitle(),
		Note:      note,
		Status:    status,
		Priority:  priority,
		ProjectID: projectID,
		Tags:      tags,
	}

	if issue.Milestone != nil && issue.Milestone.DueOn != nil {
		task.DueDate = issue.Milestone.DueOn.Format(time.RFC3339)
	}

	err := makeRequest("POST", "/task", task, nil)
	if err == nil {
		log.Printf("Created Task: %s [%s]", task.Name, status)
	}
}

func updateTaskStatus(taskID int, status string) {
	if dryRun {
		log.Printf("[DRY RUN] Would update Task %d status to %s", taskID, status)
		return
	}

	payload := map[string]string{
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
	if err != nil {
		return err
	}

	for k, v := range getHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
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