package main

import (
	"context"
	"math"
	"sort"
	"strings"
)

type platformStats struct {
	Running    running     `json:"running"`
	Workspaces *workspaces `json:"workspaces"`
}

type running struct {
	Total  int         `json:"total"`
	ByType []typeCount `json:"byType"`
}

type typeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type workspaces struct {
	Count          int     `json:"count"`
	AverageSeconds float64 `json:"averageSeconds"`
	Build          build   `json:"build"`
	// A typical WorkflowSpace sandbox build. It has too few parts to ever be the widest build.
	WorkflowBuild *build `json:"workflowBuild,omitempty"`
}

type build struct {
	TotalSeconds float64 `json:"totalSeconds"`
	Steps        []step  `json:"steps"`
}

type step struct {
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Where   string  `json:"where"`
	Seconds float64 `json:"seconds"`
}

var stepNames = map[string]string{
	"Cache":         "Cache",
	"Sql":           "SQL database",
	"Spa":           "Web app",
	"Api":           "API",
	"ObjectStorage": "Storage bucket",
	"NoSql":         "NoSQL table",
	"WorkflowSpace": "Durable workflow",
}

func (s *server) platform(ctx context.Context) (any, error) {
	var out platformStats
	counts, err := s.main.query(ctx, `count by (kind)(platform_xr_ready)`)
	if err != nil {
		return nil, err
	}
	byKind := map[string]int{}
	for _, c := range counts {
		byKind[c.labels["kind"]] = int(c.value)
	}
	// A GraphApi composes an Api of its own, so counting both counts every
	// GraphQL service twice.
	byKind["Api"] -= byKind["GraphApi"]
	for kind, n := range byKind {
		if n <= 0 {
			continue
		}
		out.Running.ByType = append(out.Running.ByType, typeCount{Type: kind, Count: n})
		out.Running.Total += n
	}
	sort.Slice(out.Running.ByType, func(i, j int) bool {
		a, b := out.Running.ByType[i], out.Running.ByType[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Type < b.Type
	})

	out.Workspaces, err = s.workspaces(ctx)
	return out, err
}

// workspaces measures Launchpad's guest workspaces, the only ones built from
// nothing on demand. A workspace is ready when its slowest piece is, so its time
// is the longest of its resources, its AWS roles and its app pods.
func (s *server) workspaces(ctx context.Context) (*workspaces, error) {
	m := s.main
	steps := map[string]map[string]step{}
	keep := func(ns string, st step) {
		if steps[ns] == nil {
			steps[ns] = map[string]step{}
		}
		if old, ok := steps[ns][st.Name]; !ok || st.Seconds > old.Seconds {
			steps[ns][st.Name] = st
		}
	}

	xrs, err := m.query(ctx, `max_over_time(platform_xr_time_to_ready_seconds_instance{exported_namespace=~"guest-.*"}[30d])`)
	if err != nil {
		return nil, err
	}
	for _, x := range xrs {
		kind := x.labels["kind"]
		name := stepNames[kind]
		if name == "" {
			name = kind
		}
		where := "pi"
		if x.labels["backend"] == "public-cloud" {
			where = "aws"
		}
		keep(x.labels["exported_namespace"], step{Name: name, Kind: kind, Where: where, Seconds: x.value})
	}

	// Identity has no resource of its own, so it counts here.
	// IAMRole: the AWS roles that grant the API its table and bucket.
	// Entra*: the Entra app a workflow's API signs in with, ready once its last part is.
	ids, err := m.query(ctx, `max_over_time(platform_managed_time_to_ready_seconds_instance{exported_namespace=~"guest-.*",kind=~"IAMRole|Entra.*"}[30d])`)
	if err != nil {
		return nil, err
	}
	for _, r := range ids {
		ns := r.labels["exported_namespace"]
		switch kind := r.labels["kind"]; {
		case kind == "IAMRole":
			keep(ns, step{Name: "Access roles", Kind: "IAMRole", Where: "aws", Seconds: r.value})
		case strings.HasPrefix(kind, "Entra"):
			keep(ns, step{Name: "Entra app", Kind: "EntraApp", Where: "entra", Seconds: r.value})
		}
	}

	pods, err := m.query(ctx, `max_over_time(platform_pod_time_to_ready_seconds_instance{exported_namespace=~"guest-.*"}[30d])`)
	if err != nil {
		return nil, err
	}
	for _, p := range pods {
		pod := p.labels["exported_pod"]
		switch {
		case strings.Contains(pod, "-api-"):
			keep(p.labels["exported_namespace"], step{Name: "API running", Kind: "Api", Where: "app", Seconds: p.value})
		case strings.Contains(pod, "-spa-"):
			keep(p.labels["exported_namespace"], step{Name: "Web app running", Kind: "Spa", Where: "app", Seconds: p.value})
		}
	}

	if len(steps) == 0 {
		return nil, nil
	}
	type ws struct {
		ns    string
		total float64
		steps []step
	}
	var all []ws
	sum := 0.0
	for ns, byName := range steps {
		w := ws{ns: ns}
		for _, st := range byName {
			w.steps = append(w.steps, st)
			w.total = math.Max(w.total, st.Seconds)
		}
		sort.Slice(w.steps, func(i, j int) bool {
			if w.steps[i].Seconds != w.steps[j].Seconds {
				return w.steps[i].Seconds < w.steps[j].Seconds
			}
			return w.steps[i].Name < w.steps[j].Name
		})
		all = append(all, w)
		sum += w.total
	}

	// The replay shows the median build among those with the most resource kinds, so it is
	// typical in time and shows the whole stack (SQL, buckets) when any workspace had it.
	sort.Slice(all, func(i, j int) bool {
		if all[i].total != all[j].total {
			return all[i].total < all[j].total
		}
		if len(all[i].steps) != len(all[j].steps) {
			return len(all[i].steps) > len(all[j].steps)
		}
		return all[i].ns < all[j].ns
	})
	// Only builds with their resource steps recorded can be replayed.
	// Some early workspaces have pod timings only.
	// A WorkflowSpace build never counts here, nor does anything with an Entra app, which only
	// WorkflowSpace builds have. A reused sandbox name can mix one with an older build, and the
	// extra steps would make that mix look like the widest build.
	var full []ws
	for _, w := range all {
		hasResource, hasWorkflow := false, false
		for _, st := range w.steps {
			hasResource = hasResource || st.Where != "app"
			hasWorkflow = hasWorkflow || st.Kind == "WorkflowSpace" || st.Kind == "EntraApp"
		}
		if hasResource && !hasWorkflow {
			full = append(full, w)
		}
	}
	if len(full) == 0 {
		full = all
	}
	widest := 0
	for _, w := range full {
		widest = max(widest, len(w.steps))
	}
	var wide []ws
	for _, w := range full {
		if len(w.steps) == widest {
			wide = append(wide, w)
		}
	}
	median := wide[(len(wide)-1)/2]

	out := &workspaces{
		Count:          len(all),
		AverageSeconds: math.Round(sum/float64(len(all))*10) / 10,
		Build:          build{TotalSeconds: median.total, Steps: median.steps},
	}

	// Sandbox names get reused, so a namespace can hold steps from an older, different build.
	// Only one made of a WorkflowSpace and its API alone counts as a WorkflowSpace build.
	workflowOnly := map[string]bool{"Durable workflow": true, "Entra app": true, "API": true, "API running": true}
	var flows []ws
	for _, w := range all {
		hasWorkflow, pure := false, true
		for _, st := range w.steps {
			hasWorkflow = hasWorkflow || st.Kind == "WorkflowSpace"
			pure = pure && workflowOnly[st.Name]
		}
		if hasWorkflow && pure {
			flows = append(flows, w)
		}
	}
	if len(flows) > 0 {
		f := flows[(len(flows)-1)/2]
		out.WorkflowBuild = &build{TotalSeconds: f.total, Steps: f.steps}
	}
	return out, nil
}
