package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// gist reads the stats.json the blog-stats workflow in github-exporter keeps
// current. GitHub stays the source of truth; the gist is a copy rebuilt hourly.
type gist struct {
	url    string
	client *http.Client
}

type automergeStats struct {
	Since     string     `json:"since"`
	Automatic int        `json:"automatic"`
	ByHand    int        `json:"byHand"`
	Updated   string     `json:"updated"`
	Days      []mergeDay `json:"days"`
}

type mergeDay struct {
	Date      string `json:"date"`
	Automatic int    `json:"automatic"`
	ByHand    int    `json:"byHand"`
}

func (g gist) automerge(ctx context.Context) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "homelab-stats")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gist returned %s", resp.Status)
	}

	var body struct {
		Files map[string]struct {
			Content string `json:"content"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode gist: %w", err)
	}
	file, ok := body.Files["stats.json"]
	if !ok {
		return nil, fmt.Errorf("gist has no stats.json")
	}

	var raw struct {
		Since    string `json:"since"`
		Renovate int    `json:"renovate"`
		Me       int    `json:"me"`
		Updated  string `json:"updated"`
		Days     []struct {
			Date string `json:"date"`
			Auto int    `json:"auto"`
			Hand int    `json:"hand"`
		} `json:"days"`
	}
	if err := json.Unmarshal([]byte(file.Content), &raw); err != nil {
		return nil, fmt.Errorf("decode stats.json: %w", err)
	}

	out := automergeStats{Since: raw.Since, Automatic: raw.Renovate, ByHand: raw.Me, Updated: raw.Updated, Days: []mergeDay{}}
	for _, d := range raw.Days {
		out.Days = append(out.Days, mergeDay{Date: d.Date, Automatic: d.Auto, ByHand: d.Hand})
	}
	return out, nil
}
