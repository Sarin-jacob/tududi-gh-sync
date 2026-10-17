package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-github/v60/github"
	"golang.org/x/oauth2"
)

// --- Configuration ---

type Config struct {
	GitHubToken   string
	GitHubAPIURL  string // optional, for GitHub Enterprise or tests
	TududiURL     string
	TududiKey     string
	Interval      time.Duration
	DryRun        bool
	Debug         bool
	RunOnce       bool
	LabelTags     bool              // add GitHub labels as Tududi tags
	DefaultArea   string            // area name for projects with no AREA_MAP entry
	AreaMap       map[string]string // lower-case repo name -> area name
	ExcludeRepos  map[string]bool   // lower-case repo names to skip
	FullSyncEvery int               // every N cycles ignore the incremental "since" window
}

func loadConfig() (*Config, error) {
	c := &Config{
		GitHubToken:  os.Getenv("GITHUB_TOKEN"),
		GitHubAPIURL: os.Getenv("GITHUB_API_URL"),
		TududiURL:    strings.TrimRight(os.Getenv("TUDUDI_URL"), "/"),
		TududiKey:    os.Getenv("TUDUDI_API_KEY"),
		DryRun:       os.Getenv("DRY_RUN") == "true",
		Debug:        os.Getenv("DEBUG") == "true",
		RunOnce:      os.Getenv("RUN_ONCE") == "true",
		LabelTags:    os.Getenv("LABEL_TAGS") != "false",
		DefaultArea:  strings.TrimSpace(os.Getenv("DEFAULT_AREA")),
		AreaMap:      parseAreaMap(os.Getenv("AREA_MAP")),
		ExcludeRepos: map[string]bool{},
	}
	if c.GitHubToken == "" || c.TududiKey == "" {
		return nil, errors.New("missing GITHUB_TOKEN or TUDUDI_API_KEY environment variables")
	}
	if c.TududiURL == "" {
		c.TududiURL = "http://localhost:3002/api/v1"
	}
	secs, err := strconv.Atoi(os.Getenv("SYNC_INTERVAL"))
	if err != nil || secs < 10 {
		secs = 300
	}
	c.Interval = time.Duration(secs) * time.Second
	c.FullSyncEvery, err = strconv.Atoi(os.Getenv("FULL_SYNC_EVERY"))
	if err != nil || c.FullSyncEvery < 1 {
		c.FullSyncEvery = 24
	}
	for _, r := range strings.Split(os.Getenv("EXCLUDE_REPOS"), ",") {
		if r = strings.ToLower(strings.TrimSpace(r)); r != "" {
			c.ExcludeRepos[r] = true
		}
	}
	return c, nil
}

// parseAreaMap reads "repo=Area Name;other-repo=Other Area".
func parseAreaMap(s string) map[string]string {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if ok && k != "" && v != "" {
			m[k] = v
		}
	}
	return m
}

// Tududi task status codes.
const (
	StatusNotStarted = 0
	StatusInProgress = 1
	StatusCompleted  = 2
	StatusArchived   = 3
	StatusWaiting    = 4
	StatusCancelled  = 5
	StatusPlanned    = 6
)

// --- Tududi data ---

type Tag struct {
	Name string `json:"name"`
}

type Area struct {
	ID   int    `json:"id"`
	UID  string `json:"uid"`
	Name string `json:"name"`
}

type Project struct {
	ID          int         `json:"id"`
	UID         string      `json:"uid"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Status      interface{} `json:"status"`
	AreaID      *int        `json:"area_id"`
}

type Task struct {
	ID           int         `json:"id"`
	UID          string      `json:"uid"`
	Name         string      `json:"name"`
	Note         string      `json:"note"`
	Status       int         `json:"status"`
	Priority     interface{} `json:"priority"`
	ProjectID    *int        `json:"project_id"`
	ParentTaskID *int        `json:"parent_task_id"`
	DueDate      *string     `json:"due_date"`
	Tags         []Tag       `json:"tags"`
}

func (p Project) status() string { return strings.ToLower(fmt.Sprint(p.Status)) }

// priorityNum maps a Tududi priority (number or string) to 0 low, 1 medium, 2 high, -1 unset.
func (t Task) priorityNum() int {
	switch v := t.Priority.(type) {
	case float64:
		return int(v)
	case string:
		return priorityValue(v)
	}
	return -1
}

func priorityValue(s string) int {
	switch strings.ToLower(s) {
	case "low":
		return 0
	case "medium":
		return 1
	case "high":
		return 2
	}
	return -1
}

var priorityNames = []string{"low", "medium", "high"}

// --- Tududi client ---

type apiError struct {
	Method, Path string
	Code         int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("tududi %s %s -> HTTP %d", e.Method, e.Path, e.Code)
}

type Tududi struct {
	base, key string
	debug     bool
	hc        *http.Client
}

func newTududi(base, key string, debug bool) *Tududi {
	return &Tududi{base: base, key: key, debug: debug, hc: &http.Client{Timeout: 60 * time.Second}}
}

// raw performs a request. GETs are retried; any non-2xx result is an error.
func (c *Tududi) raw(method, path string, body interface{}) ([]byte, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 2 * time.Second)
		}
		req, err := http.NewRequest(method, c.base+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if c.debug {
			log.Printf("[DEBUG] %s %s -> %d (%d bytes)", method, path, resp.StatusCode, len(data))
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = &apiError{method, path, resp.StatusCode}
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, &apiError{method, path, resp.StatusCode}
		}
		return data, nil
	}
	return nil, lastErr
}

// list decodes either {"<key>": [...]} or a bare array. A response of any other shape is an error,
// so an unexpected reply can never be mistaken for "there is nothing here".
func (c *Tududi) list(path, key string, out interface{}) error {
	data, err := c.raw(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	var wrapped map[string]json.RawMessage
	if json.Unmarshal(data, &wrapped) == nil {
		inner, ok := wrapped[key]
		if !ok {
			return fmt.Errorf("tududi GET %s: response has no %q field", path, key)
		}
		return json.Unmarshal(inner, out)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("tududi GET %s: unexpected response: %w", path, err)
	}
	return nil
}

// --- Sync state ---

type syncer struct {
	cfg *Config
	gh  *github.Client
	td  *Tududi

	login        string
	cycle        int
	lastStart    time.Time
	lastProjects int
	lastTasks    int

	repoCache map[string]*github.Repository
	warned    map[string]bool
	mockID    int
}

type issueRef struct {
	issue *github.Issue
	owner string
	repo  string
}

func (r issueRef) fullName() string { return r.owner + "/" + r.repo }

// index is a snapshot of what already exists in Tududi.
type index struct {
	projects map[string]*Project // normalised name -> canonical (lowest id) project
	areas    map[string]*Area    // lower-case name -> area
	byURL    map[string]*Task    // issue URL -> canonical (lowest id) task
	legacy   map[string]*Task    // "projectID|normalised title" for tasks with no issue link
	nProj    int
	nTask    int
	dupProj  int
	dupTask  int
}

type stats struct {
	created, updated, unchanged, skipped, projectsCreated, projectsUpdated int
}

var sourceRe = regexp.MustCompile(`\*\*GitHub Source\*\*: \[Issue #\d+\]\((https://[^)\s]+)\)`)

func issueKey(u string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/")) }

func normalizeName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	return strings.TrimSpace(name)
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.DryRun {
		log.Println("DRY RUN: nothing will be written to Tududi")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := &syncer{
		cfg:       cfg,
		gh:        newGitHub(ctx, cfg),
		td:        newTududi(cfg.TududiURL, cfg.TududiKey, cfg.Debug),
		repoCache: map[string]*github.Repository{},
		warned:    map[string]bool{},
		mockID:    -1,
	}
	log.Printf("Starting sync. Interval: %s", cfg.Interval)

	for {
		if err := s.runCycle(ctx); err != nil {
			log.Printf("Cycle aborted, nothing further was written: %v", err)
		}
		if cfg.RunOnce {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.Interval):
		}
	}
}

func newGitHub(ctx context.Context, cfg *Config) *github.Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.GitHubToken})
	gh := github.NewClient(oauth2.NewClient(ctx, ts))
	if cfg.GitHubAPIURL != "" {
		if u, err := url.Parse(strings.TrimRight(cfg.GitHubAPIURL, "/") + "/"); err == nil {
			gh.BaseURL = u
		}
	}
	return gh
}

// runCycle performs one sync. Any failure to read the full state of either side aborts the
// cycle before anything is written, because writing from a partial view is what creates duplicates.
func (s *syncer) runCycle(ctx context.Context) error {
	start := time.Now()
	log.Println("--- Starting Sync Cycle ---")

	if s.login == "" {
		user, _, err := s.gh.Users.Get(ctx, "")
		if err != nil {
			return fmt.Errorf("getting GitHub user: %w", err)
		}
		s.login = user.GetLogin()
	}

	var since time.Time
	if s.cycle%s.cfg.FullSyncEvery != 0 && !s.lastStart.IsZero() {
		since = s.lastStart.Add(-10 * time.Minute)
	}
	refs, complete := s.fetchIssues(ctx, since)
	log.Printf("Fetched %d GitHub issues (since: %v)", len(refs), !since.IsZero())

	idx, err := s.loadIndex()
	if err != nil {
		return err
	}
	log.Printf("Tududi has %d projects and %d tasks (%d duplicate project names, %d tasks sharing an issue link)",
		idx.nProj, idx.nTask, idx.dupProj, idx.dupTask)

	st := s.apply(refs, idx)
	log.Printf("Cycle done: %d tasks created, %d updated, %d unchanged, %d skipped; %d projects created, %d updated",
		st.created, st.updated, st.unchanged, st.skipped, st.projectsCreated, st.projectsUpdated)

	if complete {
		s.lastStart = start
	}
	s.cycle++
	return nil
}

// --- GitHub ---

// fetchIssues returns every issue worth syncing. complete is false if any GitHub call failed,
// in which case the next cycle repeats the same window.
func (s *syncer) fetchIssues(ctx context.Context, since time.Time) ([]issueRef, bool) {
	seen := map[int64]bool{}
	var refs []issueRef
	complete := true

	add := func(owner, repo string, is *github.Issue) {
		if is.IsPullRequest() || seen[is.GetID()] {
			return
		}
		seen[is.GetID()] = true
		refs = append(refs, issueRef{issue: is, owner: owner, repo: repo})
	}

	// Issues in repositories you own.
	repoOpts := &github.RepositoryListOptions{Type: "owner", ListOptions: github.ListOptions{PerPage: 100}}
	for {
		repos, resp, err := s.gh.Repositories.List(ctx, "", repoOpts)
		if err != nil {
			log.Printf("Error listing repos: %v", err)
			complete = false
			break
		}
		for _, repo := range repos {
			owner, name := repo.GetOwner().GetLogin(), repo.GetName()
			if owner != s.login || s.cfg.ExcludeRepos[strings.ToLower(name)] {
				continue
			}
			s.repoCache[owner+"/"+name] = repo
			issueOpts := &github.IssueListByRepoOptions{
				State: "all", Sort: "updated", Direction: "desc", Since: since,
				ListOptions: github.ListOptions{PerPage: 100},
			}
			for {
				issues, iresp, err := s.gh.Issues.ListByRepo(ctx, owner, name, issueOpts)
				if err != nil {
					log.Printf("Error getting issues for %s/%s: %v", owner, name, err)
					complete = false
					break
				}
				for _, is := range issues {
					add(owner, name, is)
				}
				if iresp.NextPage == 0 {
					break
				}
				issueOpts.Page = iresp.NextPage
			}
		}
		if resp.NextPage == 0 {
			break
		}
		repoOpts.Page = resp.NextPage
	}

	// Issues assigned to you in repositories you do not own.
	query := fmt.Sprintf("assignee:%s is:issue", s.login)
	if !since.IsZero() {
		query += " updated:>=" + since.UTC().Format("2006-01-02")
	}
	searchOpts := &github.SearchOptions{Sort: "updated", Order: "desc", ListOptions: github.ListOptions{PerPage: 100}}
	for page := 0; page < 10; page++ {
		result, resp, err := s.gh.Search.Issues(ctx, query, searchOpts)
		if err != nil {
			log.Printf("Error searching assigned issues: %v", err)
			complete = false
			break
		}
		for _, is := range result.Issues {
			owner, name := splitRepoURL(is.GetRepositoryURL())
			if owner == "" || owner == s.login || s.cfg.ExcludeRepos[strings.ToLower(name)] {
				continue
			}
			add(owner, name, is)
		}
		if resp.NextPage == 0 {
			break
		}
		searchOpts.Page = resp.NextPage
	}

	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].fullName() != refs[j].fullName() {
			return refs[i].fullName() < refs[j].fullName()
		}
		return refs[i].issue.GetNumber() < refs[j].issue.GetNumber()
	})
	return refs, complete
}

func splitRepoURL(u string) (owner, repo string) {
	parts := strings.Split(strings.TrimRight(u, "/"), "/")
	if len(parts) < 2 {
		return "", ""
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

// repoInfo returns repository metadata (description, archived flag), fetching foreign repos once.
func (s *syncer) repoInfo(ctx context.Context, ref issueRef) *github.Repository {
	if r, ok := s.repoCache[ref.fullName()]; ok {
		return r
	}
	r, _, err := s.gh.Repositories.Get(ctx, ref.owner, ref.repo)
	if err != nil {
		log.Printf("Could not read repo %s: %v", ref.fullName(), err)
		r = &github.Repository{Name: github.String(ref.repo)}
	}
	s.repoCache[ref.fullName()] = r
	return r
}

// --- Tududi snapshot ---

func (s *syncer) loadIndex() (*index, error) {
	var projects []Project
	if err := s.td.list("/projects?status=all", "projects", &projects); err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	var tasks []Task
	if err := s.td.list("/tasks?type=all&client_side_filtering=true", "tasks", &tasks); err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	var areas []Area
	if err := s.td.list("/areas", "areas", &areas); err != nil {
		return nil, fmt.Errorf("listing areas: %w", err)
	}

	// A sudden large drop means the listing is truncated, not that the data vanished.
	if s.lastProjects > 0 && len(projects)*2 < s.lastProjects {
		return nil, fmt.Errorf("project listing shrank from %d to %d, refusing to continue", s.lastProjects, len(projects))
	}
	if s.lastTasks > 0 && len(tasks)*2 < s.lastTasks {
		return nil, fmt.Errorf("task listing shrank from %d to %d, refusing to continue", s.lastTasks, len(tasks))
	}
	s.lastProjects, s.lastTasks = len(projects), len(tasks)

	idx := &index{
		projects: map[string]*Project{},
		areas:    map[string]*Area{},
		byURL:    map[string]*Task{},
		legacy:   map[string]*Task{},
		nProj:    len(projects),
		nTask:    len(tasks),
	}
	for i := range areas {
		idx.areas[strings.ToLower(strings.TrimSpace(areas[i].Name))] = &areas[i]
	}
	// Lowest id wins, so the oldest project and task is always the canonical one.
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	for i := range projects {
		key := normalizeName(projects[i].Name)
		if _, dup := idx.projects[key]; dup {
			idx.dupProj++
			continue
		}
		idx.projects[key] = &projects[i]
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	for i := range tasks {
		t := &tasks[i]
		if t.ParentTaskID != nil {
			continue
		}
		if m := sourceRe.FindStringSubmatch(t.Note); m != nil {
			key := issueKey(m[1])
			if _, dup := idx.byURL[key]; dup {
				idx.dupTask++
				continue
			}
			idx.byURL[key] = t
		} else if t.ProjectID != nil {
			idx.legacy[fmt.Sprintf("%d|%s", *t.ProjectID, normalizeName(t.Name))] = t
		}
	}
	return idx, nil
}

// --- Apply ---

func (s *syncer) apply(refs []issueRef, idx *index) stats {
	var st stats
	ctx := context.Background()
	for _, ref := range refs {
		repo := s.repoInfo(ctx, ref)
		proj := s.ensureProject(ref, repo, idx, &st)
		if proj == nil {
			st.skipped++
			continue
		}

		key := issueKey(ref.issue.GetHTMLURL())
		task := idx.byURL[key]
		if task == nil && proj.ID > 0 {
			task = idx.legacy[fmt.Sprintf("%d|%s", proj.ID, normalizeName(ref.issue.GetTitle()))]
			if task != nil {
				log.Printf("Adopting existing unlinked task %q for %s#%d", task.Name, ref.fullName(), ref.issue.GetNumber())
				delete(idx.legacy, fmt.Sprintf("%d|%s", proj.ID, normalizeName(ref.issue.GetTitle())))
				idx.byURL[key] = task
			}
		}
		if task != nil {
			if s.updateTask(task, ref, proj) {
				st.updated++
			} else {
				st.unchanged++
			}
			continue
		}
		if s.createTask(ref, proj, idx) {
			st.created++
		} else {
			st.skipped++
		}
	}
	return st
}

func projectNameFor(ref issueRef, login string) string {
	if ref.owner == login {
		return ref.repo
	}
	return ref.fullName()
}

func (s *syncer) areaFor(ref issueRef, idx *index) *Area {
	name := s.cfg.AreaMap[strings.ToLower(ref.repo)]
	if name == "" {
		name = s.cfg.DefaultArea
	}
	if name == "" {
		return nil
	}
	a := idx.areas[strings.ToLower(name)]
	if a == nil && !s.warned["area:"+name] {
		s.warned["area:"+name] = true
		log.Printf("Area %q (for repo %s) does not exist in Tududi, create it first", name, ref.repo)
	}
	return a
}

func legacyDescription(name string) string { return "Imported GitHub Repository: " + name }

func (s *syncer) ensureProject(ref issueRef, repo *github.Repository, idx *index, st *stats) *Project {
	name := projectNameFor(ref, s.login)
	key := normalizeName(name)
	area := s.areaFor(ref, idx)

	if p := idx.projects[key]; p != nil {
		s.fillProject(p, ref, repo, area, st)
		return p
	}

	status := "in_progress"
	if repo.GetArchived() {
		status = "done"
	}
	desc := repo.GetDescription()
	if desc == "" {
		desc = legacyDescription(name)
	}
	if repo.GetHTMLURL() != "" {
		desc += "\n\n" + repo.GetHTMLURL()
	}
	payload := map[string]interface{}{"name": name, "description": desc, "status": status, "priority": "medium"}
	if area != nil {
		payload["area_id"] = area.ID
	}

	p := &Project{Name: name, Description: desc, Status: status}
	if area != nil {
		p.AreaID = &area.ID
	}
	if s.cfg.DryRun {
		log.Printf("[DRY RUN] Would create project %q (status %s, area %s)", name, status, areaLabel(area))
		p.ID = s.mockID
		s.mockID--
	} else {
		var created Project
		data, err := s.td.raw(http.MethodPost, "/project", payload)
		if err == nil {
			err = json.Unmarshal(data, &created)
		}
		if err != nil || created.ID == 0 {
			log.Printf("Could not create project %q: %v", name, err)
			return nil
		}
		log.Printf("Created project %q (id %d)", name, created.ID)
		p.ID, p.UID = created.ID, created.UID
	}
	idx.projects[key] = p
	st.projectsCreated++
	return p
}

func areaLabel(a *Area) string {
	if a == nil {
		return "none"
	}
	return a.Name
}

// fillProject only ever fills gaps and never overwrites what you set by hand.
func (s *syncer) fillProject(p *Project, ref issueRef, repo *github.Repository, area *Area, st *stats) {
	if p.ID < 0 {
		return
	}
	patch := map[string]interface{}{}
	var what []string
	if p.AreaID == nil && area != nil {
		patch["area_id"] = area.ID
		what = append(what, "area="+area.Name)
	}
	desc := strings.TrimSpace(p.Description)
	if realDesc := repo.GetDescription(); realDesc != "" && (desc == "" || strings.EqualFold(desc, legacyDescription(p.Name))) {
		d := realDesc
		if repo.GetHTMLURL() != "" {
			d += "\n\n" + repo.GetHTMLURL()
		}
		patch["description"] = d
		what = append(what, "description")
	}
	if repo.GetArchived() {
		if cur := p.status(); cur != "done" && cur != "cancelled" {
			patch["status"] = "done"
			what = append(what, "status=done (repo archived)")
		}
	}
	if len(patch) == 0 {
		return
	}
	if s.cfg.DryRun {
		log.Printf("[DRY RUN] Would update project %q: %s", p.Name, strings.Join(what, ", "))
		st.projectsUpdated++
		return
	}
	if p.UID == "" {
		log.Printf("Cannot update project %q: missing uid", p.Name)
		return
	}
	if _, err := s.td.raw(http.MethodPatch, "/project/"+p.UID, patch); err != nil {
		log.Printf("Could not update project %q: %v", p.Name, err)
		return
	}
	log.Printf("Updated project %q: %s", p.Name, strings.Join(what, ", "))
	if a, ok := patch["area_id"].(int); ok {
		p.AreaID = &a
	}
	if d, ok := patch["description"].(string); ok {
		p.Description = d
	}
	if v, ok := patch["status"].(string); ok {
		p.Status = v
	}
	st.projectsUpdated++
}

// desired describes what a GitHub issue should look like in Tududi.
type desired struct {
	name        string
	note        string
	status      int
	priority    int // -1: no priority label, leave it alone
	due         string
	tags        []string
	projectName string
}

func (s *syncer) describe(ref issueRef, proj *Project) desired {
	is := ref.issue
	d := desired{name: is.GetTitle(), priority: -1, projectName: proj.Name}
	d.note = strings.TrimSpace(is.GetBody())
	link := fmt.Sprintf("**GitHub Source**: [Issue #%d](%s)", is.GetNumber(), is.GetHTMLURL())
	if d.note != "" {
		d.note += "\n\n"
	}
	d.note += link

	if is.GetState() == "closed" {
		d.status = StatusCompleted
	}

	var names []string
	for _, l := range is.Labels {
		names = append(names, l.GetName())
	}
	d.priority = labelPriority(names)

	if m := is.Milestone; m != nil && !m.GetDueOn().IsZero() {
		d.due = m.GetDueOn().Format("2006-01-02")
	}

	d.tags = []string{ref.repo, "github"}
	if s.cfg.LabelTags {
		for _, n := range names {
			d.tags = append(d.tags, strings.ToLower(strings.TrimSpace(n)))
		}
	}
	return d
}

// labelPriority returns 0/1/2 when a label names a priority, otherwise -1.
func labelPriority(labels []string) int {
	result := -1
	for _, l := range labels {
		tokens := strings.FieldsFunc(strings.ToLower(l), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		})
		for _, tok := range tokens {
			switch tok {
			case "urgent", "critical", "high", "blocker", "p0", "p1":
				return 2
			case "low", "minor", "trivial", "p3", "p4":
				result = 0
			}
		}
	}
	return result
}

// targetStatus decides the Tududi status for an issue. GitHub owns open/closed; the Tududi-only
// states (in progress, waiting, cancelled, archived) are yours and are not touched.
func targetStatus(current int, closed bool) (int, bool) {
	switch {
	case closed && (current == StatusNotStarted || current == StatusInProgress || current == StatusWaiting || current == StatusPlanned):
		return StatusCompleted, true
	case !closed && current == StatusCompleted:
		return StatusNotStarted, true
	}
	return current, false
}

func (s *syncer) updateTask(t *Task, ref issueRef, proj *Project) bool {
	d := s.describe(ref, proj)
	patch := map[string]interface{}{}
	var what []string

	if t.Name != d.name {
		patch["name"] = d.name
		what = append(what, fmt.Sprintf("name %q -> %q", t.Name, d.name))
	}
	if normalizeNote(t.Note) != normalizeNote(d.note) {
		patch["note"] = d.note
		what = append(what, "note")
	}
	if d.priority >= 0 && t.priorityNum() != d.priority {
		patch["priority"] = priorityNames[d.priority]
		what = append(what, "priority="+priorityNames[d.priority])
	}
	if d.due != "" && (t.DueDate == nil || firstN(*t.DueDate, 10) != d.due) {
		patch["due_date"] = d.due
		what = append(what, "due="+d.due)
	}
	if status, change := targetStatus(t.Status, ref.issue.GetState() == "closed"); change {
		patch["status"] = status
		what = append(what, fmt.Sprintf("status %d -> %d", t.Status, status))
	}
	// Tags are only ever added, never removed, so hand-made tags survive.
	have := map[string]bool{}
	var merged []Tag
	for _, tg := range t.Tags {
		have[strings.ToLower(tg.Name)] = true
		merged = append(merged, tg)
	}
	var added []string
	for _, n := range d.tags {
		if n != "" && !have[strings.ToLower(n)] {
			have[strings.ToLower(n)] = true
			merged = append(merged, Tag{Name: n})
			added = append(added, n)
		}
	}
	if len(added) > 0 {
		patch["tags"] = merged
		what = append(what, "tags+"+strings.Join(added, ","))
	}

	if len(patch) == 0 {
		return false
	}
	label := fmt.Sprintf("%s#%d %q", ref.fullName(), ref.issue.GetNumber(), t.Name)
	if s.cfg.DryRun {
		log.Printf("[DRY RUN] Would update task %s: %s", label, strings.Join(what, "; "))
		return true
	}
	if t.UID == "" {
		log.Printf("Cannot update task %s: missing uid", label)
		return false
	}
	if _, err := s.td.raw(http.MethodPatch, "/task/"+t.UID, patch); err != nil {
		log.Printf("Could not update task %s: %v", label, err)
		return false
	}
	log.Printf("Updated task %s: %s", label, strings.Join(what, "; "))
	return true
}

var (
	blankLines    = regexp.MustCompile(`\n{2,}`)
	trailingSpace = regexp.MustCompile(`[ \t]+\n`)
)

// normalizeNote ignores line-ending, blank-line and trailing-space differences so a note is not
// rewritten for whitespace alone.
func normalizeNote(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = trailingSpace.ReplaceAllString(s, "\n")
	return strings.TrimSpace(blankLines.ReplaceAllString(s, "\n\n"))
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *syncer) createTask(ref issueRef, proj *Project, idx *index) bool {
	d := s.describe(ref, proj)
	priority := "medium"
	if d.priority >= 0 {
		priority = priorityNames[d.priority]
	}
	var tags []Tag
	seen := map[string]bool{}
	for _, n := range d.tags {
		if n != "" && !seen[strings.ToLower(n)] {
			seen[strings.ToLower(n)] = true
			tags = append(tags, Tag{Name: n})
		}
	}
	payload := map[string]interface{}{
		"name": d.name, "note": d.note, "status": d.status, "priority": priority,
		"project_id": proj.ID, "tags": tags,
	}
	if d.due != "" {
		payload["due_date"] = d.due
	}
	label := fmt.Sprintf("%s#%d %q", ref.fullName(), ref.issue.GetNumber(), d.name)
	if s.cfg.DryRun {
		log.Printf("[DRY RUN] Would create task %s in %q [status %d, priority %s]", label, proj.Name, d.status, priority)
		return true
	}
	data, err := s.td.raw(http.MethodPost, "/task", payload)
	if err != nil {
		log.Printf("Could not create task %s: %v", label, err)
		return false
	}
	// Remember it for the rest of this cycle so the same issue is never created twice.
	var created Task
	if json.Unmarshal(data, &created) == nil {
		created.Note, created.Name = d.note, d.name
		idx.byURL[issueKey(ref.issue.GetHTMLURL())] = &created
	}
	log.Printf("Created task %s", label)
	return true
}
