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

// Configuration
var (
	githubToken   = os.Getenv("GITHUB_TOKEN")
	tududiURL     = strings.TrimRight(os.Getenv("TUDUDI_URL"), "/")
	tududiAPIKey  = os.Getenv("TUDUDI_API_KEY")
	syncInterval  = os.Getenv("SYNC_INTERVAL")
)

// Data Structures
type Project struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}

type Tag struct {
	Name string `json:"name"`
}

type Task struct {
	UID       string `json:"uid,omitempty"` // API might ignore this on creation, but we send it for intent
	Name      string `json:"name"`
	Note      string `json:"note"`
	Status    string `json:"status"`
	Priority  string `json:"priority"`
	ProjectID int    `json:"project_id"`
	DueDate   string `json:"due_date,omitempty"`
	Tags      []Tag  `json:"tags,omitempty"`
}

func main() {
	// 1. Basic Setup
	if githubToken == "" || tududiAPIKey == "" {
		log.Fatal("Missing GITHUB_TOKEN or TUDUDI_API_KEY environment variables.")
	}
	if tududiURL == "" {
		tududiURL = "http://localhost:3002/api/v1"
	}

	interval, err := strconv.Atoi(syncInterval)
	if err != nil || interval < 10 {
		interval = 300 // Default to 5 minutes
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: githubToken})
	tc := oauth2.NewClient(ctx, ts)
	ghClient := github.NewClient(tc)

	log.Printf("Starting Sync Service. Interval: %d seconds", interval)

	// 2. Scheduler Loop
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	// Run once immediately
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

	// Track IDs to prevent processing the same issue twice in one run
	processedIDs := make(map[int64]bool)
	var issuesToSync []*github.Issue

	// ---------------------------------------------------------
	// 3. GitHub Fetching Logic
	// ---------------------------------------------------------
	
	// A. Search Assigned Issues
	// Added 'is:issue' to fix the 422 error
	opts := &github.SearchOptions{Sort: "created", Order: "desc"}
	query := fmt.Sprintf("assignee:%s is:issue state:open", myLogin)
	
	result, _, err := gh.Search.Issues(ctx, query, opts)
	if err != nil {
		log.Printf("Error searching assigned issues: %v", err)
	} else {
		for _, issue := range result.Issues {
			if !processedIDs[issue.GetID()] {
				issuesToSync = append(issuesToSync, issue)
				processedIDs[issue.GetID()] = true
			}
		}
	}

	// B. Iterate Owned Repositories
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
				issueOpts := &github.IssueListByRepoOptions{
					State: "open", 
					ListOptions: github.ListOptions{PerPage: 50},
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

	if len(issuesToSync) == 0 {
		log.Println("No issues found to sync.")
		return
	}

	log.Printf("Found %d unique issues to process.", len(issuesToSync))
	syncIssuesToTududi(issuesToSync)
}

func syncIssuesToTududi(issues []*github.Issue) {
	// ---------------------------------------------------------
	// 4. Tududi Deduplication & Creation
	// ---------------------------------------------------------
	
	// Fetch existing data to avoid duplicates
	existingTasks := fetchTududiTasks()
	existingUIDs := make(map[string]bool)
	
	// We map UIDs to check if "gh_xxx" already exists
	for _, t := range existingTasks {
		if t.UID != "" {
			existingUIDs[t.UID] = true
		}
	}

	projects := fetchTududiProjects()
	projectMap := make(map[string]int) // Key: Normalized Name, Value: ID

	for _, p := range projects {
		projectMap[normalizeName(p.Name)] = p.ID
	}

	for _, issue := range issues {
		// Determine Repo Name and ID
		repo := issue.GetRepository()
		repoID := int64(0)
		repoName := "Unknown Repository"
		
		if repo != nil {
			repoID = repo.GetID()
			repoName = repo.GetName()
		} else if issue.RepositoryURL != nil {
			// Fallback parsing if Repository struct is empty (common in Search results)
			parts := strings.Split(*issue.RepositoryURL, "/")
			repoName = parts[len(parts)-1]
			// We can't easily get ID from URL, so we rely on issue ID for uniqueness if repo ID missing
		}
		
		// Generate Unique ID: gh_{repoID}_{issueNumber}
		tududiUID := fmt.Sprintf("gh_%d_%d", repoID, issue.GetNumber())

		// SKIP if already exists
		if existingUIDs[tududiUID] {
			continue
		}

		// Check or Create Project
		normRepoName := normalizeName(repoName)
		projID, exists := projectMap[normRepoName]
		
		if !exists {
			log.Printf("Creating new project for repo: %s", repoName)
			newID := createTududiProject(repoName)
			if newID != 0 {
				projectMap[normRepoName] = newID
				projID = newID
			} else {
				log.Printf("Skipping task creation for issue %d (Project creation failed)", issue.GetNumber())
				continue
			}
		}

		createTududiTask(issue, projID, tududiUID, repoName)
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
	// GET /api/projects (Plural is correct for fetching)
	makeRequest("GET", "/projects", nil, &projects)
	return projects
}

func fetchTududiTasks() []Task {
	// GET /api/tasks returns { "tasks": [...] } structure based on Swagger
	// We need a wrapper struct to decode it correctly
	type TaskResponse struct {
		Tasks []Task `json:"tasks"`
	}
	var resp TaskResponse
	
	// If the API returns a direct array, this might fail, but Swagger says it returns an object
	err := makeRequest("GET", "/tasks", nil, &resp)
	if err != nil {
		// Fallback: try decoding as array just in case swagger is slightly off
		var arrayResp []Task
		if makeRequest("GET", "/tasks", nil, &arrayResp) == nil {
			return arrayResp
		}
	}
	return resp.Tasks
}

func createTududiProject(name string) int {
	payload := map[string]string{
		"name":        name,
		"status":      "planned",
		"description": "Imported from GitHub",
		"priority":    "medium",
	}
	var result Project
	
	// FIX: POST /api/project (Singular)
	err := makeRequest("POST", "/project", payload, &result)
	if err != nil {
		return 0
	}
	return result.ID
}

func createTududiTask(issue *github.Issue, projectID int, uid string, repoName string) {
	note := issue.GetBody()
	note += fmt.Sprintf("\n\n**GitHub Source**: [Issue #%d](%s)", issue.GetNumber(), issue.GetHTMLURL())

	priority := "medium"
	for _, label := range issue.Labels {
		lname := strings.ToLower(label.GetName())
		if strings.Contains(lname, "urgent") || strings.Contains(lname, "high") {
			priority = "high"
		}
	}
	
	// Prepare Tags
	tags := []Tag{
		{Name: repoName},
		{Name: "github"},
	}

	task := Task{
		UID:       uid, // Trying to send UID. If API ignores it, deduplication relies on persistence
		Name:      issue.GetTitle(),
		Note:      note,
		Status:    "pending",
		Priority:  priority,
		ProjectID: projectID,
		Tags:      tags,
	}

	if issue.Milestone != nil && issue.Milestone.DueOn != nil {
		task.DueDate = issue.Milestone.DueOn.Format(time.RFC3339)
	}

	// FIX: POST /api/task (Singular)
	err := makeRequest("POST", "/task", task, nil)
	if err == nil {
		log.Printf("Created Task: %s (Project ID: %d)", task.Name, projectID)
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
		// Don't log full error for 404/duplicates if we expect them, but here we log everything for debug
		log.Printf("API Error (%d) on %s: %s", resp.StatusCode, endpoint, string(b))
		return fmt.Errorf("API Error")
	}

	if target != nil {
		return json.NewDecoder(resp.Body).Decode(target)
	}
	return nil
}