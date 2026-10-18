package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeProm answers each query with the first canned result whose key the query
// contains. Range queries repeat the value at every step.
type promResult struct {
	labels map[string]string
	value  string
}

func fakeProm(t *testing.T, answers []struct {
	match   string
	results []promResult
}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		var results []promResult
		for _, a := range answers {
			if strings.Contains(q, a.match) {
				results = a.results
				break
			}
		}
		var items []map[string]any
		for _, res := range results {
			item := map[string]any{"metric": res.labels}
			if strings.HasSuffix(r.URL.Path, "query_range") {
				start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
				end, _ := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
				step, _ := strconv.ParseInt(r.URL.Query().Get("step"), 10, 64)
				var values [][]any
				for ts := start; ts <= end; ts += step {
					values = append(values, []any{float64(ts), res.value})
				}
				item["values"] = values
			} else {
				item["value"] = []any{float64(time.Now().Unix()), res.value}
			}
			items = append(items, item)
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"result": items}}) //nolint:errcheck
	}))
}

func one(v string) []promResult { return []promResult{{labels: map[string]string{}, value: v}} }

func testServer(main, avail, gistURL string) *server {
	c := &http.Client{Timeout: 5 * time.Second}
	return &server{
		main:         prometheus{base: main, client: c},
		availability: prometheus{base: avail, client: c},
		gist:         gist{url: gistURL, client: c},
		cache:        newCache(),
		now:          func() time.Time { return time.Unix(1792300000, 0) },
	}
}

func TestClusterRejectsUnknownRange(t *testing.T) {
	s := testServer("http://unused", "http://unused", "http://unused")
	rec := httptest.NewRecorder()
	s.clusterHandler(rec, httptest.NewRequest(http.MethodGet, "/stats/cluster?range=1h", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCluster(t *testing.T) {
	node := func(name, v string) promResult {
		return promResult{labels: map[string]string{"nodename": name}, value: v}
	}
	main := fakeProm(t, []struct {
		match   string
		results []promResult
	}{
		{`kube_node_status_condition`, one("4")},
		{`count(kube_node_info)`, one("4")},
		{`count(node_cpu_seconds_total`, one("16")},
		{`count(kube_pod_status_phase`, one("145")},
		{`health_status="Healthy"`, one("44")},
		{`count(argocd_app_info)`, one("45")},
		{`sum(node_memory_MemTotal_bytes)`, one("42000000000")},
		{`node_network_receive`, one("1300000")},
		{`node_network_transmit`, one("1400000")},
		{`node_disk_read`, one("NaN")},
		{`node_disk_written`, one("1500000")},
		{`node_cpu_seconds_total{mode="idle"}`, []promResult{node("work-1", "12.345"), node("ctrl-1", "22")}},
		{`node_memory_MemAvailable_bytes`, []promResult{node("work-1", "60"), node("ctrl-1", "58")}},
		{`max by (nodename)(node_memory_MemTotal_bytes`, []promResult{node("work-1", "8000000000"), node("ctrl-1", "16000000000")}},
		{`node_hwmon_temp_celsius`, []promResult{node("work-1", "41.456"), node("ctrl-1", "44")}},
		{`kube_node_role`, []promResult{{labels: map[string]string{"node": "ctrl-1", "role": "control-plane"}, value: "1"}}},
	})
	defer main.Close()
	avail := fakeProm(t, []struct {
		match   string
		results []promResult
	}{
		{`avg_over_time`, one("99.99189814")},
		{`up{job="node-exporter"} == 1) < 3`, one("1789872300")},
		{`probe_success{probe_group="internet"}) == 0`, nil},
	})
	defer avail.Close()

	s := testServer(main.URL, avail.URL, "http://unused")
	rec := httptest.NewRecorder()
	s.clusterHandler(rec, httptest.NewRequest(http.MethodGet, "/stats/cluster?range=1d", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Last-Modified") == "" {
		t.Error("Last-Modified is missing")
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q", got)
	}
	var got clusterStats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if got.Availability30d == nil || *got.Availability30d != 99.9918 {
		t.Errorf("availability = %v, want truncated 99.9918", got.Availability30d)
	}
	// 1789872300 is 2026-09-19 22:45 in Chicago, already the 20th in UTC.
	if got.LastPowerOutage == nil || *got.LastPowerOutage != "2026-09-19" {
		t.Errorf("last power outage = %v, want the Chicago date 2026-09-19", got.LastPowerOutage)
	}
	if got.LastInternetOutage != nil {
		t.Errorf("last internet outage = %v, want null with no outage in range", *got.LastInternetOutage)
	}
	if got.Nodes != (nodeTotals{Ready: 4, Total: 4, Cores: 16, MemoryBytes: 42e9}) || got.Pods != 145 || got.Apps != (appTotals{Synced: 44, Total: 45}) {
		t.Errorf("totals = %+v pods %d apps %+v", got.Nodes, got.Pods, got.Apps)
	}
	if len(got.Timestamps) != 48 || len(got.PodSeries) != 48 {
		t.Fatalf("1d grid has %d timestamps and %d pod points, want 48", len(got.Timestamps), len(got.PodSeries))
	}
	if got.Disk.Read[0] != nil {
		t.Errorf("NaN disk read should be null, got %v", *got.Disk.Read[0])
	}
	if *got.Network.In[0] != 1300000 {
		t.Errorf("network in = %v", *got.Network.In[0])
	}
	if len(got.PerNode) != 2 || got.PerNode[0].Name != "ctrl-1" || got.PerNode[0].Role != "control-plane" || got.PerNode[1].Role != "worker" {
		t.Fatalf("per node = %+v", got.PerNode)
	}
	if *got.PerNode[1].CPU[0] != 12.35 || *got.PerNode[1].TemperatureC != 41.46 {
		t.Errorf("work-1 cpu %v temp %v, want values rounded to 2 places", *got.PerNode[1].CPU[0], *got.PerNode[1].TemperatureC)
	}
}

func TestPlatform(t *testing.T) {
	xr := func(ns, kind, backend, v string) promResult {
		return promResult{labels: map[string]string{"exported_namespace": ns, "kind": kind, "backend": backend}, value: v}
	}
	pod := func(ns, name, v string) promResult {
		return promResult{labels: map[string]string{"exported_namespace": ns, "exported_pod": name}, value: v}
	}
	kind := func(k, v string) promResult { return promResult{labels: map[string]string{"kind": k}, value: v} }
	main := fakeProm(t, []struct {
		match   string
		results []promResult
	}{
		{`count by (kind)(platform_xr_ready)`, []promResult{kind("Api", "14"), kind("GraphApi", "4"), kind("Spa", "6"), kind("Sql", "0")}},
		{`platform_xr_time_to_ready_seconds_instance`, []promResult{
			xr("guest-a", "Spa", "private-cloud", "1"), xr("guest-a", "NoSql", "public-cloud", "14"),
			xr("guest-b", "Spa", "private-cloud", "2"),
			xr("guest-c", "Cache", "private-cloud", "0"),
		}},
		{`platform_managed_time_to_ready_seconds_instance`, []promResult{
			{labels: map[string]string{"exported_namespace": "guest-a", "kind": "IAMRole"}, value: "5"},
			{labels: map[string]string{"exported_namespace": "guest-a", "kind": "IAMRole"}, value: "6"},
		}},
		{`platform_pod_time_to_ready_seconds_instance`, []promResult{
			pod("guest-a", "guest-a-api-7fdc8b59b6-nhrg2", "41"), pod("guest-a", "guest-a-spa-58db8bfcb4-wspfc", "16"),
			pod("guest-b", "guest-b-api-5b5c97fcd7-rhbs2", "30"),
			pod("guest-c", "guest-c-api-684cf67b45-6b49x", "50"),
			// Pod timings only, and the median of all four, so the replay must skip it.
			pod("guest-d", "guest-d-api-5d478c6578-lpdbh", "40"),
		}},
	})
	defer main.Close()

	s := testServer(main.URL, "http://unused", "http://unused")
	v, err := s.platform(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := v.(platformStats)

	if got.Running.Total != 20 || got.Running.ByType[0] != (typeCount{Type: "Api", Count: 10}) {
		t.Errorf("running = %+v, want 20 with Api 10 after the GraphApi dedupe", got.Running)
	}
	w := got.Workspaces
	if w == nil || w.Count != 4 || w.AverageSeconds != 40.3 {
		t.Fatalf("workspaces = %+v, want 4 averaging (41+30+50+40)/4", w)
	}
	if w.Build.TotalSeconds != 41 {
		t.Errorf("build total = %v, want the median workspace at 41", w.Build.TotalSeconds)
	}
	want := []step{
		{"Web app", "Spa", "pi", 1}, {"Access roles", "IAMRole", "aws", 6}, {"NoSQL table", "NoSql", "aws", 14},
		{"Web app running", "Spa", "app", 16}, {"API running", "Api", "app", 41},
	}
	if fmt.Sprint(w.Build.Steps) != fmt.Sprint(want) {
		t.Errorf("steps = %v, want %v", w.Build.Steps, want)
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "guest-") {
		t.Errorf("response leaks a guest namespace name: %s", body)
	}
}

func TestPlatformWithNoWorkspaces(t *testing.T) {
	main := fakeProm(t, nil)
	defer main.Close()
	v, err := testServer(main.URL, "http://unused", "http://unused").platform(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := v.(platformStats); got.Workspaces != nil {
		t.Errorf("workspaces = %+v, want null when nobody built one", got.Workspaces)
	}
}

func TestAutomerge(t *testing.T) {
	stats := `{"since":"2026-09-06","renovate":311,"me":33,"updated":"2026-10-18T17:16:51Z","days":[{"date":"2026-08-07","auto":0,"hand":9},{"date":"2026-09-06","auto":10,"hand":12}]}`
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"files": map[string]any{"stats.json": map[string]string{"content": stats}}}) //nolint:errcheck
	}))
	defer gh.Close()

	v, err := testServer("http://unused", "http://unused", gh.URL).gist.automerge(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := v.(automergeStats)
	if got.Automatic != 311 || got.ByHand != 33 || got.Since != "2026-09-06" || len(got.Days) != 2 || got.Days[1] != (mergeDay{"2026-09-06", 10, 12}) {
		t.Errorf("automerge = %+v", got)
	}
}

func TestCacheServesStaleOnError(t *testing.T) {
	c := newCache()
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	ok := func(context.Context) (any, error) { return "fresh", nil }
	fail := func(context.Context) (any, error) { return nil, errors.New("prometheus down") }

	if v, _ := c.get(context.Background(), "k", time.Minute, ok); v != "fresh" {
		t.Fatalf("first get = %v", v)
	}
	now = now.Add(2 * time.Minute)
	if v, err := c.get(context.Background(), "k", time.Minute, fail); err != nil || v != "fresh" {
		t.Errorf("stale get = %v, %v, want the last good value", v, err)
	}
	if _, err := c.get(context.Background(), "other", time.Minute, fail); err == nil {
		t.Error("a key with nothing cached should return the error")
	}
}
