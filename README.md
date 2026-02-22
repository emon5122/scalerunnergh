# ScaleRunnerGH

A fully ephemeral, GPU-aware, auto-scaling GitHub Actions runner orchestrator built with Go and Docker.

Each queued job gets its own isolated runner container that spins up on demand and self-destructs after completion. Supports multiple organizations and repositories simultaneously.

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│                    ScaleRunnerGH Orchestrator             │
│                                                          │
│  ┌──────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │   Job    │  │   Resource   │  │   Docker Runner  │   │
│  │ Monitor  │──│   Manager    │──│    Launcher      │   │
│  │ (Poller) │  │ (GPU/CPU)    │  │  (Ephemeral)     │   │
│  └────┬─────┘  └──────────────┘  └────────┬─────────┘  │
│       │                                     │            │
│  ┌────┴─────┐                    ┌─────────┴────────┐   │
│  │ GitHub   │                    │  Job Tracker &   │   │
│  │ API      │                    │  Cleanup         │   │
│  │ Client   │                    └──────────────────┘   │
│  └──────────┘                                           │
│                                                          │
│  ┌──────────────────────────────────────────────────┐   │
│  │              Prometheus Metrics (:9090)           │   │
│  └──────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────┘
         │                              │
         ▼                              ▼
┌────────────────┐           ┌────────────────┐
│ GitHub Actions │           │ Docker Daemon   │
│ API            │           │                 │
└────────────────┘           │ ┌────┐ ┌────┐  │
                             │ │ R1 │ │ R2 │  │
                             │ └────┘ └────┘  │
                             │ ┌────┐         │
                             │ │ R3 │ ...     │
                             │ └────┘         │
                             └────────────────┘
```

## Features

- **Ephemeral Isolation**: Every job runs in its own container; the host remains clean
- **Multi-Org & Multi-Repo**: Monitor jobs across multiple GitHub organizations and repositories
- **GPU-Aware Scaling**: Tracks available GPUs to prevent conflicts between GPU workloads
- **Dynamic Scaling**: Runners created on demand and destroyed automatically
- **Label-Based Routing**: Filter jobs by labels (`gpu`, `cpu`, `docker`, etc.)
- **Resource Limits**: Configurable max runners, GPUs, CPUs, and memory
- **Prometheus Metrics**: Built-in `/metrics` endpoint for monitoring
- **Grafana Dashboard**: Pre-built dashboard for runner visualization
- **Zero Dependencies**: Pure Go with no external runtime dependencies
- **Per-Repo PAT Support**: Different tokens for different orgs/repos

## Quick Start

### Option 1: Environment Variables (Single Repo)

```bash
export GITHUB_PAT=ghp_your_token_here
export GITHUB_OWNER=my-org
export GITHUB_REPO=my-app
export RUNNER_LABELS=self-hosted,gpu
export GPU_ENABLED=true
export MAX_RUNNERS=5
export MAX_GPUS=4

./scalerunnergh
```

### Option 2: Config File (Multi-Org/Repo)

```bash
cp config.example.json config.json
# Edit config.json with your repos and tokens
./scalerunnergh --config config.json
```

### Option 3: Docker Compose (Full Stack)

```bash
cp config.example.json config.json
# Edit config.json
docker-compose up -d
```

This starts the orchestrator, Prometheus, and Grafana. Access:
- Grafana: http://localhost:3000 (admin/admin)
- Prometheus: http://localhost:9091
- Metrics: http://localhost:9090/metrics
- Health: http://localhost:9090/health

## Configuration

### Config File Format

```json
{
  "repos": [
    {
      "owner": "my-org",
      "repo": "my-app",
      "labels": ["self-hosted", "gpu", "docker"]
    },
    {
      "owner": "another-org",
      "repo": "",
      "labels": ["self-hosted"],
      "pat": "ghp_org_specific_token"
    }
  ],
  "global_pat": "ghp_your_default_token",
  "poll_interval": "10s",
  "resources": {
    "max_runners": 10,
    "max_gpus": 4,
    "max_cpus": 32,
    "max_mem_mb": 65536
  },
  "docker": {
    "image": "scalerunnergh-runner:latest",
    "gpu_enabled": true,
    "network_mode": "bridge"
  },
  "metrics": {
    "enabled": true,
    "address": ":9090"
  },
  "log_level": "info"
}
```

### Org-Level Runners

Set `repo` to an empty string to register runners at the organization level:

```json
{
  "owner": "my-org",
  "repo": "",
  "labels": ["self-hosted"]
}
```

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `GITHUB_PAT` | GitHub Personal Access Token | required |
| `GITHUB_OWNER` | GitHub org or user | required |
| `GITHUB_REPO` | Repository name | optional (org-level if empty) |
| `RUNNER_LABELS` | Comma-separated labels | `self-hosted` |
| `POLL_INTERVAL` | How often to check for jobs | `10s` |
| `MAX_RUNNERS` | Max concurrent runners | `5` |
| `MAX_GPUS` | Max GPUs to allocate | `0` (unlimited) |
| `RUNNER_IMAGE` | Docker image for runners | `ghcr.io/actions/runner:latest` |
| `GPU_ENABLED` | Enable GPU passthrough | `false` |
| `METRICS_ADDR` | Metrics server address | `:9090` |
| `LOG_LEVEL` | Log level | `info` |

## Building

### From Source

```bash
go build -o scalerunnergh ./cmd/scalerunnergh
```

### Docker Image (Orchestrator)

```bash
docker build -t scalerunnergh .
```

### Docker Image (Runner)

```bash
docker build -t scalerunnergh-runner docker/runner/
```

## Testing

```bash
# Unit tests
go test ./... -v

# E2E integration tests (mock GitHub API)
go test -tags integration ./tests/integration/ -v

# With real GitHub API (requires GITHUB_PAT)
GITHUB_PAT=ghp_xxx go test -tags integration ./tests/integration/ -v
```

## Metrics

The `/metrics` endpoint exposes Prometheus-compatible metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `scalerunner_active_runners` | gauge | Currently running containers |
| `scalerunner_queued_jobs` | gauge | Total queued jobs across all repos |
| `scalerunner_total_launched` | counter | Total runners launched |
| `scalerunner_total_completed` | counter | Runners completed successfully |
| `scalerunner_total_failed` | counter | Runners that failed |
| `scalerunner_used_gpus` | gauge | GPUs currently in use |
| `scalerunner_available_gpus` | gauge | GPUs available |
| `scalerunner_poll_errors_total` | counter | GitHub API poll errors |
| `scalerunner_launch_errors_total` | counter | Runner launch errors |
| `scalerunner_token_errors_total` | counter | Registration token errors |
| `scalerunner_repo_queued_jobs` | gauge | Queued jobs per repo |

## Project Structure

```
scalerunnergh/
├── cmd/scalerunnergh/       # CLI entrypoint
│   └── main.go
├── internal/
│   ├── config/              # Multi-org/repo config system
│   ├── github/              # GitHub API client
│   ├── metrics/             # Prometheus metrics
│   ├── orchestrator/        # Main polling loop & job dispatch
│   ├── resource/            # GPU/CPU/memory resource manager
│   └── runner/              # Docker container launcher
├── tests/integration/       # E2E & integration tests
├── docker/
│   └── runner/              # Ephemeral runner Dockerfile
├── deploy/
│   ├── prometheus/          # Prometheus config
│   └── grafana/             # Grafana dashboards & provisioning
├── Dockerfile               # Orchestrator image
├── docker-compose.yml       # Full monitoring stack
└── config.example.json      # Example configuration
```

## How It Works

1. **Poll**: The orchestrator polls GitHub Actions API for queued workflow jobs at a configurable interval
2. **Match**: Jobs are filtered by label to determine which runner config handles them
3. **Token**: A short-lived registration token is generated via the GitHub API
4. **Allocate**: The resource manager checks if there's capacity (runner slots, GPUs, etc.)
5. **Launch**: A Docker container is created with `--ephemeral` runner configuration
6. **Execute**: The runner picks up the job, executes it, and exits
7. **Cleanup**: Resources are released, the container is removed, and metrics are updated

## GitHub Token Permissions

Create a fine-grained PAT with these permissions:
- **Repository**: `Actions: Read and write` (for listing runs/jobs and creating registration tokens)
- **Organization**: `Self-hosted runners: Read and write` (for org-level runners)

## License

MIT
