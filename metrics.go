package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Counters in Prometheus text format, written by hand to stay stdlib only.
var (
	metricsMu    sync.Mutex
	requests     = map[string]int{}
	staleServed  = map[string]int{}
	requestLabel = func(method, path string, code int) string {
		return fmt.Sprintf(`method="%s",path="%s",code="%d"`, method, path, code)
	}
)

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// countRequests labels by the matched route pattern so scanner paths do not create new series.
func countRequests(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		_, pattern := mux.Handler(r)
		if pattern == "" || pattern == "/" {
			pattern = "other"
		}
		metricsMu.Lock()
		requests[requestLabel(r.Method, pattern, rec.code)]++
		metricsMu.Unlock()
	})
}

func countStale(key string) {
	metricsMu.Lock()
	staleServed[key]++
	metricsMu.Unlock()
}

func metricsHandler(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	metricsMu.Lock()
	b.WriteString("# TYPE homelab_stats_requests_total counter\n")
	for _, k := range sortedKeys(requests) {
		fmt.Fprintf(&b, "homelab_stats_requests_total{%s} %d\n", k, requests[k])
	}
	b.WriteString("# TYPE homelab_stats_stale_served_total counter\n")
	for _, k := range sortedKeys(staleServed) {
		fmt.Fprintf(&b, "homelab_stats_stale_served_total{key=%q} %d\n", k, staleServed[k])
	}
	metricsMu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
