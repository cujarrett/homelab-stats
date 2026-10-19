# List available recipes
default:
    @just --list

# Run all CI checks locally before pushing
ci: lint test build

# Verify go.mod is tidy and run golangci-lint
lint:
    go mod tidy -diff
    golangci-lint run

# Run tests with race detector
test:
    go test -race ./...

# Build binary
build:
    go build -o homelab-stats .

# Run locally
run:
    go run .

# Run against the cluster's Prometheus on :18080, for the site's npm run start:local
dev:
    #!/usr/bin/env bash
    set -euo pipefail
    # kubectl port-forward drops after a while, so each one restarts itself.
    forward() { while true; do kubectl port-forward -n monitoring "svc/$1" "$2:9090" >/dev/null 2>&1; sleep 1; done; }
    forward monitoring-kube-prometheus-prometheus 19090 &
    forward prometheus-cluster-availability 19091 &
    trap 'kill 0' EXIT
    sleep 2
    PORT=18080 PROMETHEUS_URL=http://localhost:19090 AVAILABILITY_PROMETHEUS_URL=http://localhost:19091 go run .
