package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"
	_ "time/tzdata" // the runtime image has no zoneinfo, and outage dates are Chicago days
)

// version is set at build time via -ldflags="-X main.version=x.y.z".
// Falls back to "dev" when running locally with go run.
var version = "dev"

// The gist the blog-stats workflow in github-exporter writes every hour.
const defaultGistID = "bae61e2f9258ad06f4f123b138803a4c"

func main() {
	port := envOr("PORT", "8080")
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	httpClient := &http.Client{Timeout: 15 * time.Second}
	s := &server{
		main:         prometheus{base: envOr("PROMETHEUS_URL", "http://monitoring-kube-prometheus-prometheus.monitoring.svc.cluster.local:9090"), client: httpClient},
		availability: prometheus{base: envOr("AVAILABILITY_PROMETHEUS_URL", "http://prometheus-cluster-availability.monitoring.svc.cluster.local:9090"), client: httpClient},
		gist:         gist{url: "https://api.github.com/gists/" + envOr("STATS_GIST_ID", defaultGistID), client: httpClient},
		cache:        newCache(),
		now:          time.Now,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("GET /stats/cluster", s.clusterHandler)
	mux.HandleFunc("GET /stats/platform", s.platformHandler)
	mux.HandleFunc("GET /stats/automerge", s.automergeHandler)
	mux.HandleFunc("/", notFoundHandler)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() {
		logger.Info("server listening", "port", port, "version", version)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	if err := srv.Shutdown(context.Background()); err != nil {
		logger.Error("shutdown error", "err", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type server struct {
	main         prometheus
	availability prometheus
	gist         gist
	cache        *cache
	now          func() time.Time
}

// Every response is public and identical for every caller, so Cloudflare may
// hold it for the same minute this process does.
func writeStats(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeJSONError(w, "stats are unavailable right now", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Write(body) //nolint:errcheck
}

func (s *server) clusterHandler(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("range")
	if key == "" {
		key = "1d"
	}
	rng, ok := clusterRanges[key]
	if !ok {
		writeJSONError(w, "range must be one of 1d, 1w, 1m", http.StatusBadRequest)
		return
	}
	v, err := s.cache.get(r.Context(), "cluster-"+key, time.Minute, func(ctx context.Context) (any, error) {
		return s.cluster(ctx, key, rng)
	})
	if err != nil {
		slog.Error("cluster stats", "range", key, "err", err)
		writeJSONError(w, "stats are unavailable right now", http.StatusBadGateway)
		return
	}
	writeStats(w, v)
}

func (s *server) platformHandler(w http.ResponseWriter, r *http.Request) {
	v, err := s.cache.get(r.Context(), "platform", 5*time.Minute, s.platform)
	if err != nil {
		slog.Error("platform stats", "err", err)
		writeJSONError(w, "stats are unavailable right now", http.StatusBadGateway)
		return
	}
	writeStats(w, v)
}

func (s *server) automergeHandler(w http.ResponseWriter, r *http.Request) {
	// The gist changes at most hourly, and GitHub allows 60 unauthenticated reads an hour.
	v, err := s.cache.get(r.Context(), "automerge", 10*time.Minute, s.gist.automerge)
	if err != nil {
		slog.Error("automerge stats", "err", err)
		writeJSONError(w, "stats are unavailable right now", http.StatusBadGateway)
		return
	}
	writeStats(w, v)
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","version":%q}`, version) //nolint:errcheck
}

func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	slog.Info("404", "method", r.Method, "path", r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
}

func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Write(b) //nolint:errcheck
}
