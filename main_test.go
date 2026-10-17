package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v60/github"
)

// fakeTududi is an in-memory stand-in for the Tududi REST API that records every write.
type fakeTududi struct {
	mu          sync.Mutex
	projects    []map[string]interface{}
	tasks       []map[string]interface{}
	areas       []map[string]interface{}
	failProject bool
	writes      []string
	nextID      int
}

func (f *fakeTududi) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	var body map[string]interface{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	write := func(v interface{}) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == "GET" && path == "/projects":
		if f.failProject {
			http.Error(w, "boom", 500)
			return
		}
		write(map[string]interface{}{"projects": f.projects})
	case r.Method == "GET" && path == "/tasks":
		write(map[string]interface{}{"tasks": f.tasks})
	case r.Method == "GET" && path == "/areas":
		if f.areas == nil {
			f.areas = []map[string]interface{}{}
		}
		write(f.areas)
	case r.Method == "POST" && path == "/project":
		f.writes = append(f.writes, "POST /project "+fmt.Sprint(body["name"]))
		f.nextID++
		p := map[string]interface{}{"id": 1000 + f.nextID, "uid": fmt.Sprintf("p%d", f.nextID), "name": body["name"]}
		f.projects = append(f.projects, p)
		write(p)
	case r.Method == "POST" && path == "/task":
		f.writes = append(f.writes, "POST /task "+fmt.Sprint(body["name"]))
		f.nextID++
		write(map[string]interface{}{"id": 2000 + f.nextID, "uid": fmt.Sprintf("t%d", f.nextID)})
	case r.Method == "PATCH":
		b, _ := json.Marshal(body)
		f.writes = append(f.writes, "PATCH "+path+" "+string(b))
		write(map[string]interface{}{})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeTududi) writesMatching(prefix string) []string {
	var out []string
	for _, w := range f.writes {
		if strings.HasPrefix(w, prefix) {
			out = append(out, w)
		}
	}
	return out
}

type fakeIssue struct {
	number int
	title  string
	body   string
	state  string
	labels []string
}

// fakeGitHub serves one user ("me") owning repos, each with issues.
func fakeGitHub(repos map[string][]fakeIssue, archived map[string]bool, descs map[string]string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"login": "me"})
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]interface{}
		for name := range repos {
			out = append(out, map[string]interface{}{
				"name": name, "owner": map[string]interface{}{"login": "me"},
				"archived": archived[name], "description": descs[name],
				"html_url": "https://github.com/me/" + name,
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"total_count": 0, "items": []interface{}{}})
	})
	mux.HandleFunc("/repos/me/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[3] != "issues" {
			http.NotFound(w, r)
			return
		}
		var out []map[string]interface{}
		for _, is := range repos[parts[2]] {
			var labels []map[string]interface{}
			for _, l := range is.labels {
				labels = append(labels, map[string]interface{}{"name": l})
			}
			out = append(out, map[string]interface{}{
				"id": int64(len(parts[2]))*1000 + int64(is.number), "number": is.number, "title": is.title,
				"body": is.body, "state": is.state, "labels": labels,
				"html_url": fmt.Sprintf("https://github.com/me/%s/issues/%d", parts[2], is.number),
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	return httptest.NewServer(mux)
}

func newTestSyncer(t *testing.T, gh *httptest.Server, td *httptest.Server, mutate func(*Config)) *syncer {
	t.Helper()
	cfg := &Config{
		GitHubToken: "x", GitHubAPIURL: gh.URL, TududiURL: td.URL + "/api/v1", TududiKey: "k",
		LabelTags: true, FullSyncEvery: 24, AreaMap: map[string]string{}, ExcludeRepos: map[string]bool{},
	}
	if mutate != nil {
		mutate(cfg)
	}
	return &syncer{
		cfg: cfg, gh: newGitHub(context.Background(), cfg), td: newTududi(cfg.TududiURL, "k", false),
		repoCache: map[string]*github.Repository{}, warned: map[string]bool{}, mockID: -1,
	}
}

func link(repo string, n int) string {
	return fmt.Sprintf("**GitHub Source**: [Issue #%d](https://github.com/me/%s/issues/%d)", n, repo, n)
}

func task(id int, name, repo string, n int, status int, project int) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "uid": fmt.Sprintf("u%d", id), "name": name, "note": link(repo, n), "status": status,
		"project_id": project, "priority": 1, "tags": []map[string]string{{"name": repo}, {"name": "github"}},
	}
}

func project(id int, name string, area interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "uid": fmt.Sprintf("p%d", id), "name": name, "status": "in_progress", "area_id": area,
		"description": "Imported GitHub Repository: " + name,
	}
}

func run(t *testing.T, s *syncer) error {
	t.Helper()
	return s.runCycle(context.Background())
}

func TestNoDuplicatesWhenProjectsAndTasksExist(t *testing.T) {
	gh := fakeGitHub(map[string][]fakeIssue{"Billy": {{1, "First", "", "open", nil}}}, nil, nil)
	defer gh.Close()
	// Two projects share the name: the lowest id must be used and nothing must be created.
	td := &fakeTududi{
		projects: []map[string]interface{}{project(9, "Billy", nil), project(5, "billy", nil)},
		tasks:    []map[string]interface{}{task(11, "First", "Billy", 1, 0, 5)},
	}
	srv := httptest.NewServer(td)
	defer srv.Close()

	if err := run(t, newTestSyncer(t, gh, srv, nil)); err != nil {
		t.Fatal(err)
	}
	if got := td.writesMatching("POST"); len(got) != 0 {
		t.Fatalf("expected no creates, got %v", got)
	}
}

func TestAbortsWhenProjectListFails(t *testing.T) {
	gh := fakeGitHub(map[string][]fakeIssue{"Billy": {{1, "First", "", "open", nil}}}, nil, nil)
	defer gh.Close()
	td := &fakeTududi{failProject: true}
	srv := httptest.NewServer(td)
	defer srv.Close()

	s := newTestSyncer(t, gh, srv, nil)
	if err := run(t, s); err == nil {
		t.Fatal("expected the cycle to abort")
	}
	if len(td.writes) != 0 {
		t.Fatalf("expected no writes, got %v", td.writes)
	}
}

func TestExistingTaskInAnotherProjectIsNotRecreated(t *testing.T) {
	gh := fakeGitHub(map[string][]fakeIssue{"Billy": {{1, "First", "", "open", nil}}}, nil, nil)
	defer gh.Close()
	// The task already lives in an unrelated project: it must be found by its issue link.
	td := &fakeTududi{
		projects: []map[string]interface{}{project(5, "Billy", nil), project(7, "Other", nil)},
		tasks:    []map[string]interface{}{task(11, "First", "Billy", 1, 0, 7)},
	}
	srv := httptest.NewServer(td)
	defer srv.Close()

	if err := run(t, newTestSyncer(t, gh, srv, nil)); err != nil {
		t.Fatal(err)
	}
	if got := td.writesMatching("POST /task"); len(got) != 0 {
		t.Fatalf("task was recreated: %v", got)
	}
}

func TestRenamedIssueUpdatesInsteadOfDuplicating(t *testing.T) {
	gh := fakeGitHub(map[string][]fakeIssue{"Billy": {{1, "New title", "", "open", nil}}}, nil, nil)
	defer gh.Close()
	td := &fakeTududi{
		projects: []map[string]interface{}{project(5, "Billy", nil)},
		tasks:    []map[string]interface{}{task(11, "Old title", "Billy", 1, 0, 5)},
	}
	srv := httptest.NewServer(td)
	defer srv.Close()

	if err := run(t, newTestSyncer(t, gh, srv, nil)); err != nil {
		t.Fatal(err)
	}
	if len(td.writesMatching("POST")) != 0 {
		t.Fatalf("unexpected create: %v", td.writes)
	}
	if p := td.writesMatching("PATCH /task/u11"); len(p) != 1 || !strings.Contains(p[0], "New title") {
		t.Fatalf("expected a rename patch, got %v", td.writes)
	}
}

func TestStatusPolicy(t *testing.T) {
	cases := []struct {
		name       string
		current    int
		closed     bool
		want       int
		wantChange bool
	}{
		{"closed issue completes open task", StatusNotStarted, true, StatusCompleted, true},
		{"closed issue completes in-progress task", StatusInProgress, true, StatusCompleted, true},
		{"closed issue leaves cancelled task", StatusCancelled, true, StatusCancelled, false},
		{"closed issue leaves archived task", StatusArchived, true, StatusArchived, false},
		{"open issue reopens a completed task", StatusCompleted, false, StatusNotStarted, true},
		{"open issue leaves in-progress task", StatusInProgress, false, StatusInProgress, false},
		{"open issue leaves waiting task", StatusWaiting, false, StatusWaiting, false},
		{"open issue leaves cancelled task", StatusCancelled, false, StatusCancelled, false},
	}
	for _, c := range cases {
		got, change := targetStatus(c.current, c.closed)
		if got != c.want || change != c.wantChange {
			t.Errorf("%s: got (%d,%v) want (%d,%v)", c.name, got, change, c.want, c.wantChange)
		}
	}
}

func TestPriorityOnlyFromLabels(t *testing.T) {
	if labelPriority([]string{"bug", "enhancement"}) != -1 {
		t.Error("no priority label must leave priority alone")
	}
	if labelPriority([]string{"priority: high"}) != 2 || labelPriority([]string{"P1"}) != 2 {
		t.Error("high labels")
	}
	if labelPriority([]string{"Low priority"}) != 0 {
		t.Error("low label")
	}
	if labelPriority([]string{"low", "urgent"}) != 2 {
		t.Error("high must beat low")
	}
}

func TestFillsAreaAndDescriptionButNotOverwrites(t *testing.T) {
	gh := fakeGitHub(
		map[string][]fakeIssue{"Billy": {{1, "First", "", "open", nil}}, "Kept": {{1, "Only", "", "open", nil}}},
		nil, map[string]string{"Billy": "Billing app", "Kept": "Real description"},
	)
	defer gh.Close()
	keptProject := project(6, "Kept", 4)
	keptProject["description"] = "my own text"
	td := &fakeTududi{
		areas:    []map[string]interface{}{{"id": 3, "uid": "a3", "name": "Affybo"}, {"id": 1, "uid": "a1", "name": "Company/Work"}},
		projects: []map[string]interface{}{project(5, "Billy", nil), keptProject},
		tasks: []map[string]interface{}{
			task(11, "First", "Billy", 1, 0, 5), task(12, "Only", "Kept", 1, 0, 6),
		},
	}
	srv := httptest.NewServer(td)
	defer srv.Close()

	s := newTestSyncer(t, gh, srv, func(c *Config) {
		c.AreaMap = map[string]string{"billy": "Affybo", "kept": "Company/Work"}
	})
	if err := run(t, s); err != nil {
		t.Fatal(err)
	}
	billy := td.writesMatching("PATCH /project/p5")
	if len(billy) != 1 || !strings.Contains(billy[0], `"area_id":3`) || !strings.Contains(billy[0], "Billing app") {
		t.Fatalf("Billy should get area and description: %v", td.writes)
	}
	if kept := td.writesMatching("PATCH /project/p6"); len(kept) != 0 {
		t.Fatalf("Kept already has an area and a description: %v", kept)
	}
}

func TestDryRunNeverWrites(t *testing.T) {
	gh := fakeGitHub(map[string][]fakeIssue{"Fresh": {{1, "New", "body", "open", []string{"urgent"}}}}, nil, nil)
	defer gh.Close()
	td := &fakeTududi{}
	srv := httptest.NewServer(td)
	defer srv.Close()

	if err := run(t, newTestSyncer(t, gh, srv, func(c *Config) { c.DryRun = true })); err != nil {
		t.Fatal(err)
	}
	if len(td.writes) != 0 {
		t.Fatalf("dry run wrote: %v", td.writes)
	}
}

func TestCreatesProjectAndTasks(t *testing.T) {
	gh := fakeGitHub(map[string][]fakeIssue{"Fresh": {{1, "New", "body", "open", []string{"high"}}, {2, "Done", "", "closed", nil}}}, nil, nil)
	defer gh.Close()
	td := &fakeTududi{}
	srv := httptest.NewServer(td)
	defer srv.Close()

	if err := run(t, newTestSyncer(t, gh, srv, nil)); err != nil {
		t.Fatal(err)
	}
	if n := len(td.writesMatching("POST /project")); n != 1 {
		t.Fatalf("expected 1 project, got %d: %v", n, td.writes)
	}
	if n := len(td.writesMatching("POST /task")); n != 2 {
		t.Fatalf("expected 2 tasks, got %d: %v", n, td.writes)
	}
}

func TestNoteWhitespaceIsNotAChange(t *testing.T) {
	a := "body\r\n\r\n\r\n**GitHub Source**: [Issue #1](u)"
	b := "body\n\n**GitHub Source**: [Issue #1](u)\n"
	if normalizeNote(a) != normalizeNote(b) {
		t.Fatalf("%q != %q", normalizeNote(a), normalizeNote(b))
	}
}
