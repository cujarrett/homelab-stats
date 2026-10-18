package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

type clusterRange struct {
	span   time.Duration
	step   time.Duration
	window time.Duration
}

// About 50 points per range, so each sparkline has the same amount of detail.
// 1m stops at 30 days, the main Prometheus retention.
var clusterRanges = map[string]clusterRange{
	"1d": {span: 24 * time.Hour, step: 30 * time.Minute, window: 30 * time.Minute},
	"1w": {span: 7 * 24 * time.Hour, step: 3 * time.Hour, window: 3 * time.Hour},
	"1m": {span: 30 * 24 * time.Hour, step: 12 * time.Hour, window: 12 * time.Hour},
}

type clusterStats struct {
	Range              string     `json:"range"`
	Availability30d    *float64   `json:"availability30d"`
	LastPowerOutage    *string    `json:"lastPowerOutage"`
	LastInternetOutage *string    `json:"lastInternetOutage"`
	Nodes              nodeTotals `json:"nodes"`
	Pods               int        `json:"pods"`
	Apps               appTotals  `json:"apps"`
	Timestamps         []int64    `json:"timestamps"`
	PodSeries          []*float64 `json:"podSeries"`
	PerNode            []nodeStat `json:"perNode"`
	Network            network    `json:"network"`
	Disk               disk       `json:"disk"`
}

type nodeTotals struct {
	Ready       int     `json:"ready"`
	Total       int     `json:"total"`
	Cores       int     `json:"cores"`
	MemoryBytes float64 `json:"memoryBytes"`
}

type appTotals struct {
	Synced int `json:"synced"`
	Total  int `json:"total"`
}

type nodeStat struct {
	Name         string     `json:"name"`
	Role         string     `json:"role"`
	MemoryBytes  float64    `json:"memoryBytes"`
	TemperatureC *float64   `json:"temperatureC"`
	CPU          []*float64 `json:"cpu"`
	Memory       []*float64 `json:"memory"`
}

type network struct {
	In  []*float64 `json:"in"`
	Out []*float64 `json:"out"`
}

type disk struct {
	Read  []*float64 `json:"read"`
	Write []*float64 `json:"write"`
}

type availabilityStats struct {
	Percent30d   *float64
	LastPower    *string
	LastInternet *string
}

// Matches the cluster-availability dashboard: up when at least 3 of 4 nodes answer
// and the internet probe succeeds. The offset term forgives a single missed scrape.
const availabilityUp = `clamp_max((count(up{job="node-exporter"} == 1) >= bool 3) + (count(up{job="node-exporter"} offset 30s == 1) >= bool 3), 1) * (max(probe_success{probe_group="internet"}) == bool 1)`

// Joins node-exporter series to the node's hostname, so names never come from IPs.
const byNode = `* on(instance) group_left(nodename) node_uname_info`

func (s *server) cluster(ctx context.Context, key string, rng clusterRange) (clusterStats, error) {
	out := clusterStats{Range: key}

	// Outage history takes a second to compute and changes rarely, so it caches
	// on its own. The rest of the section still renders if that archive is down.
	avail, err := s.cache.get(ctx, "availability", 10*time.Minute, func(ctx context.Context) (any, error) {
		return s.availabilityStats(ctx)
	})
	if a, ok := avail.(availabilityStats); ok && err == nil {
		out.Availability30d, out.LastPowerOutage, out.LastInternetOutage = a.Percent30d, a.LastPower, a.LastInternet
	}

	m := s.main
	instant := []struct {
		expr string
		dst  *int
	}{
		{`sum(kube_node_status_condition{condition="Ready",status="true"})`, &out.Nodes.Ready},
		{`count(kube_node_info)`, &out.Nodes.Total},
		{`count(node_cpu_seconds_total{mode="idle"})`, &out.Nodes.Cores},
		{`count(kube_pod_status_phase{phase="Running"} == 1)`, &out.Pods},
		{`count(argocd_app_info)`, &out.Apps.Total},
		{`count(argocd_app_info{health_status="Healthy",sync_status="Synced"})`, &out.Apps.Synced},
	}
	for _, q := range instant {
		v, _, err := m.scalar(ctx, q.expr)
		if err != nil {
			return out, err
		}
		*q.dst = int(v)
	}
	out.Nodes.MemoryBytes, _, err = m.scalar(ctx, `sum(node_memory_MemTotal_bytes)`)
	if err != nil {
		return out, err
	}

	end := s.now().Truncate(rng.step)
	start := end.Add(-rng.span + rng.step)
	for ts := start; !ts.After(end); ts = ts.Add(rng.step) {
		out.Timestamps = append(out.Timestamps, ts.Unix())
	}
	w := fmt.Sprintf("%ds", int(rng.window.Seconds()))
	rangeOne := func(expr string) ([]*float64, error) {
		res, err := m.queryRange(ctx, expr, start, end, rng.step)
		if err != nil || len(res) == 0 {
			return make([]*float64, len(out.Timestamps)), err
		}
		return onGrid(res[0], out.Timestamps), nil
	}
	if out.PodSeries, err = rangeOne(`count(kube_pod_status_phase{phase="Running"} == 1)`); err != nil {
		return out, err
	}
	if out.Network.In, err = rangeOne(`sum(rate(node_network_receive_bytes_total{device="eth0"}[` + w + `]))`); err != nil {
		return out, err
	}
	if out.Network.Out, err = rangeOne(`sum(rate(node_network_transmit_bytes_total{device="eth0"}[` + w + `]))`); err != nil {
		return out, err
	}
	if out.Disk.Read, err = rangeOne(`sum(rate(node_disk_read_bytes_total{device=~"nvme.n."}[` + w + `]))`); err != nil {
		return out, err
	}
	if out.Disk.Write, err = rangeOne(`sum(rate(node_disk_written_bytes_total{device=~"nvme.n."}[` + w + `]))`); err != nil {
		return out, err
	}

	out.PerNode, err = s.perNode(ctx, start, end, rng, w, out.Timestamps)
	return out, err
}

func (s *server) perNode(ctx context.Context, start, end time.Time, rng clusterRange, w string, grid []int64) ([]nodeStat, error) {
	m := s.main
	nodes := map[string]*nodeStat{}
	node := func(name string) *nodeStat {
		if nodes[name] == nil {
			nodes[name] = &nodeStat{Name: name, Role: "worker"}
		}
		return nodes[name]
	}

	cpu, err := m.queryRange(ctx, `100 * (1 - avg by (nodename)(rate(node_cpu_seconds_total{mode="idle"}[`+w+`]) `+byNode+`))`, start, end, rng.step)
	if err != nil {
		return nil, err
	}
	for _, sr := range cpu {
		node(sr.labels["nodename"]).CPU = onGrid(sr, grid)
	}
	mem, err := m.queryRange(ctx, `100 * (1 - max by (nodename)(node_memory_MemAvailable_bytes `+byNode+`) / max by (nodename)(node_memory_MemTotal_bytes `+byNode+`))`, start, end, rng.step)
	if err != nil {
		return nil, err
	}
	for _, sr := range mem {
		node(sr.labels["nodename"]).Memory = onGrid(sr, grid)
	}
	total, err := m.query(ctx, `max by (nodename)(node_memory_MemTotal_bytes `+byNode+`)`)
	if err != nil {
		return nil, err
	}
	for _, sm := range total {
		node(sm.labels["nodename"]).MemoryBytes = sm.value
	}
	temps, err := m.query(ctx, `max by (nodename)(node_hwmon_temp_celsius `+byNode+`)`)
	if err != nil {
		return nil, err
	}
	for _, sm := range temps {
		t := round2(sm.value)
		node(sm.labels["nodename"]).TemperatureC = &t
	}
	roles, err := m.query(ctx, `kube_node_role{role="control-plane"}`)
	if err != nil {
		return nil, err
	}
	for _, sm := range roles {
		if n := nodes[sm.labels["node"]]; n != nil {
			n.Role = "control-plane"
		}
	}

	out := make([]nodeStat, 0, len(nodes))
	for _, n := range nodes {
		if n.Name == "" {
			continue
		}
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *server) availabilityStats(ctx context.Context) (availabilityStats, error) {
	var out availabilityStats
	a := s.availability
	pct, ok, err := a.scalar(ctx, `avg_over_time((`+availabilityUp+`)[30d:30s]) * 100`)
	if err != nil {
		return out, err
	}
	if ok {
		// Truncated, never rounded, so a bad month cannot round up to a better one.
		r := math.Floor(pct*10000) / 10000
		out.Percent30d = &r
	}
	// Day only. The exact minute the house lost power is nobody else's business.
	day := func(expr string) (*string, error) {
		ts, ok, err := a.scalar(ctx, expr)
		if err != nil || !ok {
			return nil, err
		}
		d := time.Unix(int64(ts), 0).In(chicago).Format("2006-01-02")
		return &d, nil
	}
	if out.LastPower, err = day(`max_over_time((timestamp(count(up{job="node-exporter"} == 1) < 3))[90d:30s])`); err != nil {
		return out, err
	}
	out.LastInternet, err = day(`max_over_time((timestamp(max(probe_success{probe_group="internet"}) == 0))[90d:30s])`)
	return out, err
}

var chicago = mustLoadLocation("America/Chicago")

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}
