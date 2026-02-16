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

// Tududi Structs based on Swagger
type Project struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}

type Task struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	Status    string `json:"status"`
	Priority  string `json:"priority"`
	ProjectID int    `json:"project_id"`
	DueDate   string `json:"due_date,omitempty"`
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
		interval = 300 // Default 5 minutes
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: githubToken})
	tc := oauth2.NewClient(ctx, ts)
	ghClient := github.NewClient(tc)

	log.Printf("Starting Sync Service. Interval: %d seconds", interval)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	// Run immediately on start
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

	// Track processed GitHub Issue IDs to avoid duplication in this run
	processedIDs := make(map[int64]bool)
	var issuesToSync []*github.Issue

	// 1. Fetch Issues Assigned to Me
	opts := &github.SearchOptions{
		Sort:  "created",
		Order: "desc",
	}
	query := fmt.Sprintf("assignee:%s state:open", myLogin)
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

	// 2. Fetch All Issues from Owned Repositories
	repoOpts := &github.RepositoryListOptions{
		Type: "owner",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	repos, _, err := gh.Repositories.List(ctx, "", repoOpts)
	if err != nil {
		log.Printf("Error listing repos: %v", err)
	} else {
		for _, repo := range repos {
			// Only process if I am the owner
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
					// Issues.ListByRepo can return Pull Requests too, skip them
					if issue.IsPullRequest() {
						continue
					}
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
	existingTasks := fetchTududiTasks()
	existingUIDs := make(map[string]bool)
	for _, t := range existingTasks {
		existingUIDs[t.UID] = true
	}

	// Cache projects to avoid excessive API calls
	projects := fetchTududiProjects()
	projectMap := make(map[string]int) // Normalized Name -> ID

	for _, p := range projects {
		projectMap[normalizeName(p.Name)] = p.ID
	}

	for _, issue := range issues {
		// Construct unique ID for Tududi
		repo := issue.GetRepository()
		// Sometimes search results don't fully populate Repository struct, handle gracefully
		repoID := int64(0)
		repoName := "Unknown Repository"
		
		if repo != nil {
			repoID = repo.GetID()
			repoName = repo.GetName()
		} else {
			// Fallback if repository object is missing in search result (rare but possible in some API versions)
			// We might need to parse URL, but for now we skip or log warning
			if issue.RepositoryURL != nil {
				parts := strings.Split(*issue.RepositoryURL, "/")
				repoName = parts[len(parts)-1]
			}
		}
		
		tududiUID := fmt.Sprintf("gh_%d_%d", repoID, issue.GetNumber())

		// Skip if task exists
		if existingUIDs[tududiUID] {
			continue
		}

		// Handle Project Logic
		normRepoName := normalizeName(repoName)
		projID, exists := projectMap[normRepoName]
		
		if !exists {
			// Create new project
			log.Printf("Creating new project for repo: %s", repoName)
			newID := createTududiProject(repoName)
			if newID != 0 {
				projectMap[normRepoName] = newID
				projID = newID
			} else {
				log.Printf("Skipping task creation for issue %d due to project creation failure", issue.GetNumber())
				continue
			}
		}

		createTududiTask(issue, projID, tududiUID)
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
	makeRequest("GET", "/projects", nil, &projects)
	return projects
}

func fetchTududiTasks() []Task {
	var tasks []Task
	makeRequest("GET", "/tasks", nil, &tasks)
	return tasks
}

func createTududiProject(name string) int {
	payload := map[string]string{
		"name":        name,
		"status":      "planned",
		"description": "Imported from GitHub",
	}
	var result Project
	err := makeRequest("POST", "/projects", payload, &result)
	if err != nil {
		return 0
	}
	return result.ID
}

func createTududiTask(issue *github.Issue, projectID int, uid string) {
	note := issue.GetBody()
	note += fmt.Sprintf("\n\n**GitHub Source**: [Issue #%d](%s)", issue.GetNumber(), issue.GetHTMLURL())

	priority := "medium"
	for _, label := range issue.Labels {
		lname := strings.ToLower(label.GetName())
		if strings.Contains(lname, "urgent") || strings.Contains(lname, "high") {
			priority = "high"
		}
	}

	task := Task{
		UID:       uid,
		Name:      issue.GetTitle(),
		Note:      note,
		Status:    "pending",
		Priority:  priority,
		ProjectID: projectID,
	}

	if issue.Milestone != nil && issue.Milestone.DueOn != nil {
		task.DueDate = issue.Milestone.DueOn.Format(time.RFC3339)
	}

	err := makeRequest("POST", "/tasks", task, nil)
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
		log.Printf("API Error (%d) on %s: %s", resp.StatusCode, endpoint, string(b))
		return fmt.Errorf("API Error")
	}

	if target != nil {
		return json.NewDecoder(resp.Body).Decode(target)
	}
	return nil
}