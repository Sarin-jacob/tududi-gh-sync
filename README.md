# Tududi GitHub Sync

A small Go service that keeps **Tududi** in step with your **GitHub** issues. One GitHub repository becomes one Tududi project, and one issue becomes one task.

## How it works

Each cycle (default every 5 minutes) it:

1. Reads the issues in repositories you own, and issues assigned to you in other repositories. It follows pagination, so no issue is missed. After the first cycle it only asks for issues changed since the last one, with a full pass every `FULL_SYNC_EVERY` cycles.
2. Reads **all** projects, tasks and areas from Tududi.
3. Matches each issue to a task by its **issue link**, which the sync writes into the task note (`**GitHub Source**: [Issue #12](https://github.com/...)`). A renamed issue updates its task and never creates a second one. A task is found whichever project it sits in.
4. Creates what is missing and fills what is empty.

### Safety rules

- If Tududi's project, task or area list cannot be read, or looks truncated (it shrank to less than half of the previous cycle), the cycle stops before writing anything. It never works from a partial view.
- A project is matched by repository name (case, `-` and `_` ignored). If several projects share a name, the oldest one (lowest id) is used.
- It never deletes anything.
- `DRY_RUN=true` logs every create and update without sending any of them.

### What GitHub controls and what you control

| Field | Behaviour |
|---|---|
| Title, note (issue body + link) | GitHub is the source. Edit these on GitHub. |
| Done / not started | GitHub is the source. A closed issue completes the task; a reopened issue reopens a completed task. |
| In progress, waiting, cancelled, archived | Yours. The sync never changes them. |
| Priority | Set from labels (`urgent`, `critical`, `high`, `blocker`, `p0`, `p1` → high; `low`, `minor`, `trivial`, `p3`, `p4` → low). With no priority label it is `medium` at creation and then left alone. |
| Due date | From the milestone due date. Never cleared. |
| Tags | The repository name, `github`, and the issue labels (`LABEL_TAGS=false` turns labels off). Tags are only added, never removed. |
| Project area, description | Filled only if empty (see `AREA_MAP`). Never overwritten. |
| Archived repository | Its project is marked done. |

Because GitHub owns open/closed, close an issue on GitHub to finish the task. If you mark a task done in Tududi while its issue is still open, the next cycle reopens it.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `GITHUB_TOKEN` | required | Personal access token. Needs `repo` (private) or `public_repo`; fine-grained: Issues read-only and Metadata read-only. |
| `TUDUDI_API_KEY` | required | Tududi API token. |
| `TUDUDI_URL` | `http://localhost:3002/api/v1` | Base URL of the Tududi API. |
| `SYNC_INTERVAL` | `300` | Seconds between cycles. |
| `AREA_MAP` | empty | Area for each repo, as `repo=Area Name;other=Other Area`. The area must already exist in Tududi. |
| `DEFAULT_AREA` | empty | Area for repos not in `AREA_MAP`. |
| `EXCLUDE_REPOS` | empty | Comma-separated repo names to skip. |
| `LABEL_TAGS` | `true` | Add issue labels as tags. |
| `FULL_SYNC_EVERY` | `24` | Do a full pass every N cycles. |
| `RUN_ONCE` | `false` | Run one cycle and exit. |
| `DRY_RUN` | `false` | Log changes without writing them. |
| `DEBUG` | `false` | Log every Tududi request. |
| `GITHUB_API_URL` | empty | GitHub Enterprise or test server base URL. |

## Run

Docker Compose:

```bash
docker compose up -d --build
```

Manual:

```bash
export GITHUB_TOKEN=... TUDUDI_API_KEY=... TUDUDI_URL=https://tududi.example.com/api/v1
DRY_RUN=true RUN_ONCE=true go run .   # look at the log first
go run .
```

Test:

```bash
go test ./...
```

## License

[MIT](https://choosealicense.com/licenses/mit/)
