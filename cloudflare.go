package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"
)

// cloudflare counts the requests Cloudflare turned away before they reached the cluster.
// The token is read per use, because a rotated value lands in the file without a restart.
type cloudflare struct {
	base      string
	tokenFile string
	client    *http.Client
}

// errNoToken means this run has no token, such as a laptop. The page then shows no WAF share.
var errNoToken = errors.New("no Cloudflare token")

// The Free plan has no security event log, but its request analytics say which rule acted.
// US only, to match the map. A challenge a visitor passed shows as a different action, so it is not counted.
// Cloudflare names no attacker city, only the data centre that stopped the request.
const blockedQuery = `query($zones: [String!], $since: Time!, $until: Time!) {
  viewer { zones(filter: {zoneTag_in: $zones}) {
    httpRequestsAdaptiveGroups(limit: 10000, filter: {
      datetime_geq: $since, datetime_lt: $until, clientCountryName: "US",
      securityAction_in: ["block", "managed_challenge", "challenge", "jschallenge"]
    }) { count dimensions { datetimeHour coloCode } }
  } }
}`

// edgeBlock is the requests one data centre stopped in one hour.
type edgeBlock struct {
	Hour  time.Time
	Colo  string
	Count int
}

// blocked reads every zone the token can see, so a new site counts once the token covers it.
func (c cloudflare) blocked(ctx context.Context, since, until time.Time) ([]edgeBlock, error) {
	raw, err := os.ReadFile(c.tokenFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoToken
	}
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(raw))

	var zones struct {
		Result []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := c.call(ctx, token, http.MethodGet, "/zones?per_page=50", nil, &zones); err != nil {
		return nil, fmt.Errorf("list zones: %w", err)
	}
	ids := make([]string, 0, len(zones.Result))
	for _, z := range zones.Result {
		ids = append(ids, z.ID)
	}

	body, err := json.Marshal(map[string]any{
		"query": blockedQuery,
		"variables": map[string]any{
			"zones": ids,
			"since": since.UTC().Format(time.RFC3339),
			"until": until.UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Data struct {
			Viewer struct {
				Zones []struct {
					Groups []struct {
						Count      int `json:"count"`
						Dimensions struct {
							Hour time.Time `json:"datetimeHour"`
							Colo string    `json:"coloCode"`
						} `json:"dimensions"`
					} `json:"httpRequestsAdaptiveGroups"`
				} `json:"zones"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.call(ctx, token, http.MethodPost, "/graphql", body, &answer); err != nil {
		return nil, fmt.Errorf("query analytics: %w", err)
	}
	if len(answer.Errors) > 0 {
		return nil, fmt.Errorf("query analytics: %s", answer.Errors[0].Message)
	}
	var out []edgeBlock
	for _, z := range answer.Data.Viewer.Zones {
		for _, g := range z.Groups {
			out = append(out, edgeBlock{Hour: g.Dimensions.Hour, Colo: g.Dimensions.Colo, Count: g.Count})
		}
	}
	return out, nil
}

// Where each US data centre sits, rounded to one decimal like every attacker city.
// A data centre not listed here still counts in the totals, with no dot on the map.
var colos = map[string]attackPlace{
	"ATL": {City: "Atlanta", Region: "GA", Lat: 33.7, Lon: -84.4, Edge: true},
	"AUS": {City: "Austin", Region: "TX", Lat: 30.3, Lon: -97.7, Edge: true},
	"BNA": {City: "Nashville", Region: "TN", Lat: 36.2, Lon: -86.8, Edge: true},
	"BOS": {City: "Boston", Region: "MA", Lat: 42.4, Lon: -71.1, Edge: true},
	"BUF": {City: "Buffalo", Region: "NY", Lat: 42.9, Lon: -78.9, Edge: true},
	"CLT": {City: "Charlotte", Region: "NC", Lat: 35.2, Lon: -80.8, Edge: true},
	"CMH": {City: "Columbus", Region: "OH", Lat: 40.0, Lon: -83.0, Edge: true},
	"DEN": {City: "Denver", Region: "CO", Lat: 39.7, Lon: -105.0, Edge: true},
	"DFW": {City: "Dallas", Region: "TX", Lat: 32.8, Lon: -96.8, Edge: true},
	"DTW": {City: "Detroit", Region: "MI", Lat: 42.3, Lon: -83.0, Edge: true},
	"EWR": {City: "Newark", Region: "NJ", Lat: 40.7, Lon: -74.2, Edge: true},
	"IAD": {City: "Ashburn", Region: "VA", Lat: 39.0, Lon: -77.5, Edge: true},
	"IAH": {City: "Houston", Region: "TX", Lat: 29.8, Lon: -95.4, Edge: true},
	"IND": {City: "Indianapolis", Region: "IN", Lat: 39.8, Lon: -86.2, Edge: true},
	"JAX": {City: "Jacksonville", Region: "FL", Lat: 30.3, Lon: -81.7, Edge: true},
	"LAS": {City: "Las Vegas", Region: "NV", Lat: 36.2, Lon: -115.1, Edge: true},
	"LAX": {City: "Los Angeles", Region: "CA", Lat: 34.1, Lon: -118.2, Edge: true},
	"MCI": {City: "Kansas City", Region: "MO", Lat: 39.1, Lon: -94.6, Edge: true},
	"MEM": {City: "Memphis", Region: "TN", Lat: 35.1, Lon: -90.0, Edge: true},
	"MIA": {City: "Miami", Region: "FL", Lat: 25.8, Lon: -80.2, Edge: true},
	"MSP": {City: "Minneapolis", Region: "MN", Lat: 45.0, Lon: -93.3, Edge: true},
	"OMA": {City: "Omaha", Region: "NE", Lat: 41.3, Lon: -96.0, Edge: true},
	"ORD": {City: "Chicago", Region: "IL", Lat: 41.9, Lon: -87.6, Edge: true},
	"PDX": {City: "Portland", Region: "OR", Lat: 45.5, Lon: -122.7, Edge: true},
	"PHL": {City: "Philadelphia", Region: "PA", Lat: 40.0, Lon: -75.2, Edge: true},
	"PHX": {City: "Phoenix", Region: "AZ", Lat: 33.4, Lon: -112.1, Edge: true},
	"PIT": {City: "Pittsburgh", Region: "PA", Lat: 40.4, Lon: -80.0, Edge: true},
	"RDU": {City: "Raleigh", Region: "NC", Lat: 35.8, Lon: -78.6, Edge: true},
	"RIC": {City: "Richmond", Region: "VA", Lat: 37.5, Lon: -77.4, Edge: true},
	"SAN": {City: "San Diego", Region: "CA", Lat: 32.7, Lon: -117.2, Edge: true},
	"SEA": {City: "Seattle", Region: "WA", Lat: 47.6, Lon: -122.3, Edge: true},
	"SFO": {City: "San Francisco", Region: "CA", Lat: 37.8, Lon: -122.4, Edge: true},
	"SJC": {City: "San Jose", Region: "CA", Lat: 37.3, Lon: -121.9, Edge: true},
	"SLC": {City: "Salt Lake City", Region: "UT", Lat: 40.8, Lon: -111.9, Edge: true},
	"SMF": {City: "Sacramento", Region: "CA", Lat: 38.6, Lon: -121.5, Edge: true},
	"STL": {City: "St. Louis", Region: "MO", Lat: 38.6, Lon: -90.2, Edge: true},
	"TPA": {City: "Tampa", Region: "FL", Lat: 28.0, Lon: -82.5, Edge: true},
}

func (c cloudflare) call(ctx context.Context, token, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cloudflare returned %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
