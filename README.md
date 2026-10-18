# homelab-stats

Read-only JSON for the homelab section on [mattjarrett.dev](https://mattjarrett.dev). Every number comes from a fixed list of Prometheus queries or the blog-stats gist, and nothing from a request reaches a query except the choice of range.

## Commands

| Command | What it does |
|---|---|
| `just ci` | Lint + test + build (run before pushing) |
| `just run` | Start the server locally on port 8080 |
| `just test` | Run tests with race detector |
| `just lint` | go mod tidy -diff + golangci-lint |

To run against the cluster from a laptop, port-forward both Prometheus instances and point the app at them:

```bash
kubectl port-forward -n monitoring svc/monitoring-kube-prometheus-prometheus 19090:9090 &
kubectl port-forward -n monitoring svc/prometheus-cluster-availability 19091:9090 &
PROMETHEUS_URL=http://localhost:19090 AVAILABILITY_PROMETHEUS_URL=http://localhost:19091 just run
```

## Endpoints

The site reaches these at `https://mattjarrett.dev/api/stats/...`. Its nginx forwards only that prefix, so nothing else here is public.

| Method | Path | Cached | Description |
|---|---|---|---|
| `GET` | `/healthz` | | Readiness probe |
| `GET` | `/stats/cluster?range=1h\|1d\|1w` | 1 min | Availability, outage dates, node, pod and app totals, CPU and memory per node, network and disk throughput |
| `GET` | `/stats/platform` | 5 min | Platform resources running by type, and Launchpad workspace build times over 30 days |
| `GET` | `/stats/automerge` | 10 min | Dependency PR merges per day, automatic and by hand, from 30 days before automerge went live |

A failed refresh serves the last good answer, so a Prometheus restart does not empty the section. Responses carry `Cache-Control: public, max-age=60` for Cloudflare.

Outages are reported as a date only, and responses carry no guest workspace or pod names.

## Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `PORT` | No | `8080` | Listen port |
| `PROMETHEUS_URL` | No | `http://monitoring-kube-prometheus-prometheus.monitoring.svc.cluster.local:9090` | Main Prometheus |
| `AVAILABILITY_PROMETHEUS_URL` | No | `http://prometheus-cluster-availability.monitoring.svc.cluster.local:9090` | Long-term availability archive |
| `STATS_GIST_ID` | No | `bae61e2f9258ad06f4f123b138803a4c` | Gist the [blog-stats workflow](https://github.com/cujarrett/github-exporter#blog-stats) writes |

## Deployment

Runs on the homelab cluster as an `Api` in the `mattjarrett-dev` workspace, beside the site that proxies to it. Image: `ghcr.io/cujarrett/homelab-stats`. ARM64.
