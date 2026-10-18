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
		name := stepNames[x.labels["kind"]]
		if name == "" {
			name = x.labels["kind"]
		}
		where := "pi"
		if x.labels["backend"] == "public-cloud" {
			where = "aws"
		}
		keep(x.labels["exported_namespace"], step{Name: name, Kind: x.labels["kind"], Where: where, Seconds: x.value})
	}

	// Tables and buckets already count through their own resource. The roles that
	// grant the API access to them have no resource of their own, so they count here.
	roles, err := m.query(ctx, `max_over_time(platform_managed_time_to_ready_seconds_instance{exported_namespace=~"guest-.*",kind="IAMRole"}[30d])`)
	if err != nil {
		return nil, err
	}
	for _, r := range roles {
		keep(r.labels["exported_namespace"], step{Name: "Access roles", Kind: "IAMRole", Where: "aws", Seconds: r.value})
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

	// The replay shows the median build, so it is typical rather than the best case.
	sort.Slice(all, func(i, j int) bool {
		if all[i].total != all[j].total {
			return all[i].total < all[j].total
		}
		if len(all[i].steps) != len(all[j].steps) {
			return len(all[i].steps) > len(all[j].steps)
		}
		return all[i].ns < all[j].ns
	})
	median := all[(len(all)-1)/2]

	return &workspaces{
		Count:          len(all),
		AverageSeconds: math.Round(sum/float64(len(all))*10) / 10,
		Build:          build{TotalSeconds: median.total, Steps: median.steps},
	}, nil
}
