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
)

// --- Data Structures ---
type Project struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	UID         string `json:"uid,omitempty"`
}

type Tag struct {
	Name string `json:"name"`
}

type Task struct {
	ID        int    `json:"id,omitempty"` // ID is needed for updates
	UID       string `json:"uid,omitempty"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	Status    string `json:"status"` // pending, completed, archived
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

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: githubToken})
	tc := oauth2.NewClient(ctx, ts)
	ghClient := github.NewClient(tc)

	log.Printf("Starting Sync Service. Interval: %d seconds", interval)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	// Initial Run
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

	// 1. Fetch Issues (Removed 'state:open' so we get closed ones too)
	// We sort by updated to ensure we catch status changes
	opts := &github.SearchOptions{Sort: "updated", Order: "desc"}
	
	// Query: Assigned to me AND is an Issue (not PR)
	query := fmt.Sprintf("assignee:%s is:issue", myLogin)
	
	result, _, err := gh.Search.Issues(ctx, query, opts)
	if err != nil {
		log.Printf("Error searching issues: %v", err)
	} else {
		// Limit to first 50 updated issues to save API quota on repeated runs
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

	// 2. Fetch Owned Repositories (Also fetching closed issues to sync state)
	repoOpts := &github.RepositoryListOptions{
		Type: "owner", 
		ListOptions: github.ListOptions{PerPage: 100},
	}
	repos, _, err := gh.Repositories.List(ctx, "", repoOpts)
	if err != nil {
		log.Printf("Error listing repos: %v", err)
	} else {
		for _, repo := range repos {
			if repo.GetOwner().GetLogin() == myLogin {
				// Fetch recent issues (open and closed)
				issueOpts := &github.IssueListByRepoOptions{
					State: "all", // Fetch open and closed
					Sort:  "updated",
					Direction: "desc",
					ListOptions: github.ListOptions{PerPage: 20}, // Get last 20 updated per repo
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

	log.Printf("Processing %d issues...", len(issuesToSync))
	syncIssuesToTududi(issuesToSync)
}

func syncIssuesToTududi(issues []*github.Issue) {
	// Map Existing Tasks: UID -> Task Object
	existingTasks := fetchTududiTasks()
	existingTaskMap := make(map[string]Task)
	
	for _, t := range existingTasks {
		if t.UID != "" {
			existingTaskMap[t.UID] = t
		}
	}

	// Map Projects: Normalized Name -> ID
	projects := fetchTududiProjects()
	projectMap := make(map[string]int)

	for _, p := range projects {
		projectMap[normalizeName(p.Name)] = p.ID
	}

	for _, issue := range issues {
		repo := issue.GetRepository()
		
		// Determine Repo Name and Details
		var repoID int64
		var repoName, repoDesc string
		
		if repo != nil {
			repoID = repo.GetID()
			repoName = repo.GetName()
			repoDesc = repo.GetDescription()
		} else {
			// Fallback parsing
			if issue.RepositoryURL != nil {
				parts := strings.Split(*issue.RepositoryURL, "/")
				repoName = parts[len(parts)-1]
			}
			repoDesc = fmt.Sprintf("Imported GitHub Repository: %s", repoName)
		}
		
		tududiUID := fmt.Sprintf("gh_%d_%d", repoID, issue.GetNumber())
		
		// Determine target statuses
		targetStatus := "pending"
		if issue.GetState() == "closed" {
			targetStatus = "completed"
		}

		// --- CHECK 1: Does Task Exist? ---
		if task, exists := existingTaskMap[tududiUID]; exists {
			// Task exists. Check if we need to update status.
			// Tududi Statuses: pending, completed, archived
			// GitHub Statuses: open, closed
			
			needsUpdate := false
			
			if targetStatus == "completed" && task.Status == "pending" {
				needsUpdate = true
			} else if targetStatus == "pending" && (task.Status == "completed" || task.Status == "archived") {
				needsUpdate = true
			}

			if needsUpdate {
				log.Printf("Updating Status for '%s': %s -> %s", task.Name, task.Status, targetStatus)
				updateTaskStatus(task.ID, targetStatus)
			}
			continue // Skip to next issue
		}

		// --- CHECK 2: Create New Task ---
		
		// Find Project ID
		normRepoName := normalizeName(repoName)
		projID, exists := projectMap[normRepoName]
		
		if !exists {
			log.Printf("Project '%s' not found locally. Creating...", repoName)
			newID := createTududiProject(repoName, repoDesc)
			if newID != 0 {
				projectMap[normRepoName] = newID
				projID = newID
			} else {
				log.Printf("Skipping issue %d (Project creation failed)", issue.GetNumber())
				continue
			}
		}

		// Create the task
		createTududiTask(issue, projID, tududiUID, repoName, targetStatus)
	}
}

// --- Tududi API Helpers ---

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
	var projects []Project
	// ADDED: ?status=all
	// Without this, Tududi might hide 'done' or 'planned' projects, causing duplicates.
	err := makeRequest("GET", "/projects?status=all", nil, &projects)
	if err != nil {
		log.Printf("Warning: Failed to fetch projects: %v", err)
	}
	return projects
}

func fetchTududiTasks() []Task {
	// Added filtering to get ALL tasks, including completed ones, so we don't recreate them
	type TaskResponse struct {
		Tasks []Task `json:"tasks"`
	}
	var resp TaskResponse
	
	// Fetch 'all' types (pending, completed, archived)
	err := makeRequest("GET", "/tasks?type=all", nil, &resp)
	if err != nil {
		// Fallback for different API structures
		var arrayResp []Task
		if makeRequest("GET", "/tasks?type=all", nil, &arrayResp) == nil {
			return arrayResp
		}
	}
	return resp.Tasks
}

func createTududiProject(name, description string) int {
	if description == "" {
		description = "Imported from GitHub"
	}
	
	payload := map[string]interface{}{
		"name":        name,
		"status":      "planned",
		"description": description,
		"priority":    "medium",
		// "image_url": "...", // If you had a URL, you could add it here
	}
	var result Project
	
	err := makeRequest("POST", "/project", payload, &result)
	if err != nil {
		return 0
	}
	return result.ID
}

func createTududiTask(issue *github.Issue, projectID int, uid string, repoName string, status string) {
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
		Status:    status, // Set initial status based on GH state
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
	// Using the specific toggle endpoint isn't ideal for setting exact state,
	// so we use the PATCH endpoint to force specific fields.
	payload := map[string]string{
		"status": status,
	}
	
	endpoint := fmt.Sprintf("/task/%d", taskID)
	err := makeRequest("PATCH", endpoint, payload, nil)
	if err != nil {
		log.Printf("Failed to update task %d: %v", taskID, err)
	}
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
		log.Printf("Request failed %s: %v", endpoint, err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		// Only log 404s if we are not expecting them (like checking for existence)
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