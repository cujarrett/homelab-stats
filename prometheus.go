package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// prometheus is a minimal client for the two query endpoints. Every expression
// it runs is a constant in this repo, never text from a request.
type prometheus struct {
	base   string
	client *http.Client
}

type sample struct {
	labels map[string]string
	value  float64
}

type series struct {
	labels map[string]string
	points map[int64]float64
}

type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
			Values [][]any           `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (p prometheus) get(ctx context.Context, path string, params url.Values) (promResponse, error) {
	var out promResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path+"?"+params.Encode(), nil)
	if err != nil {
		return out, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode %s: %w", path, err)
	}
	if out.Status != "success" {
		return out, fmt.Errorf("prometheus %s: %s", path, out.Error)
	}
	return out, nil
}

func (p prometheus) query(ctx context.Context, expr string) ([]sample, error) {
	resp, err := p.get(ctx, "/api/v1/query", url.Values{"query": {expr}})
	if err != nil {
		return nil, err
	}
	var out []sample
	for _, r := range resp.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		v, ok := parseValue(r.Value[1])
		if !ok {
			continue
		}
		out = append(out, sample{labels: r.Metric, value: v})
	}
	return out, nil
}

func (p prometheus) queryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]series, error) {
	resp, err := p.get(ctx, "/api/v1/query_range", url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.Itoa(int(step.Seconds()))},
	})
	if err != nil {
		return nil, err
	}
	var out []series
	for _, r := range resp.Data.Result {
		s := series{labels: r.Metric, points: map[int64]float64{}}
		for _, pair := range r.Values {
			if len(pair) != 2 {
				continue
			}
			ts, ok := pair[0].(float64)
			if !ok {
				continue
			}
			if v, ok := parseValue(pair[1]); ok {
				s.points[int64(ts)] = v
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// parseValue drops NaN and Inf, which Prometheus returns for empty ratios and
// encoding/json refuses to write.
func parseValue(raw any) (float64, bool) {
	str, ok := raw.(string)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(str, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// scalar returns the single value of a query that aggregates to one series.
func (p prometheus) scalar(ctx context.Context, expr string) (float64, bool, error) {
	samples, err := p.query(ctx, expr)
	if err != nil || len(samples) == 0 {
		return 0, false, err
	}
	return samples[0].value, true, nil
}

// onGrid lines a series up with the shared timestamps, leaving gaps as null.
func onGrid(s series, grid []int64) []*float64 {
	out := make([]*float64, len(grid))
	for i, ts := range grid {
		if v, ok := s.points[ts]; ok {
			r := round2(v)
			out[i] = &r
		}
	}
	return out
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
