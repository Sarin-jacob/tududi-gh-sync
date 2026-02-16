# Tududi GitHub Sync

A lightweight, containerized Go service that keeps your **Tududi** task board in sync with your **GitHub** issues.

It automatically fetches issues assigned to you (or from repositories you own), creates the corresponding Projects in Tududi, and keeps the task status in sync (Open ↔ Pending, Closed ↔ Completed).

## Features

* **Smart Sync:** Fetches issues assigned to you *and* issues from repositories you own.
* **Auto-Project Creation:** Automatically creates a Project in Tududi for every GitHub Repository found (if it doesn't exist).
* **Status Mapping:**
* GitHub `Open` → Tududi `Pending` (Status 0)
* GitHub `Closed` → Tududi `Completed` (Status 2)


* **Rich Metadata:** Populates tasks with the issue body, priority (based on labels like "urgent"), due dates (from milestones), and tags.
* **Deduplication:** Uses strict Name + Project matching to prevent duplicate tasks.
* **Archive Support:** If a GitHub repository is Archived, the Tududi Project is created with a "Done" status.

## Setup

### Prerequisites

1. **GitHub Token:** A Personal Access Token (classic or fine-grained).
* *Recommended Scope:* `repo` (for private) or just `public_repo`. If using Fine-grained, ensure `Issues: Read-only`.


2. **Tududi API Key:** Your Bearer token or API Key from your Tududi instance.

### Docker Compose (Recommended)

Create a `docker-compose.yml` file:

```yaml
version: '3.8'

services:
  tududi-gh-sync:
    build: .
    container_name: tududi-gh-sync
    restart: unless-stopped
    environment:
      - GITHUB_TOKEN=ghp_your_github_token_here
      - TUDUDI_API_KEY=your_tududi_api_key_here
      # If Tududi is on the same machine:
      - TUDUDI_URL=http://host.docker.internal:3002/api/v1
      # Check every 5 minutes (300 seconds)
      - SYNC_INTERVAL=300
      # Optional Debugging
      - DRY_RUN=false
      - DEBUG=false

```

Run it:

```bash
docker-compose up -d --build

```

### Manual Run (Go)

```bash
# 1. Export variables
export GITHUB_TOKEN="ghp_..."
export TUDUDI_API_KEY="ey..."
export TUDUDI_URL="http://localhost:3002/api/v1"

# 2. Run
go run main.go

```

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `GITHUB_TOKEN` | **Required** | Your GitHub Personal Access Token. |
| `TUDUDI_API_KEY` | **Required** | Your Tududi Authentication Token. |
| `TUDUDI_URL` | `http://localhost:3002/api/v1` | The base URL of your Tududi API. |
| `SYNC_INTERVAL` | `300` | How often to check GitHub (in seconds). |
| `DRY_RUN` | `false` | If `true`, logs what *would* happen without modifying data. |
| `DEBUG` | `false` | If `true`, prints raw API responses and detailed logs. |

## Building From Source

The project uses a multi-stage Dockerfile to ensure a tiny image size (Alpine Linux based).

**Dockerfile:**

```dockerfile
# Build Stage
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN go build -o sync-tool main.go

# Run Stage
FROM alpine:latest
WORKDIR /app
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/sync-tool .
CMD ["./sync-tool"]

```

## Deduplication Logic

To avoid creating duplicate tasks:

1. The sync tool creates a unique "fingerprint" for every task based on `Project ID` + `Normalized Task Name`.
2. It checks if this fingerprint exists in Tududi before creating a new task.
3. If it matches, it checks if the **Status** needs updating (e.g., you closed the issue on GitHub) and patches it using the Task UID.

## Contributing

Pull requests are welcome. For major changes, please open an issue first to discuss what you would like to change.

## License

[MIT](https://choosealicense.com/licenses/mit/)