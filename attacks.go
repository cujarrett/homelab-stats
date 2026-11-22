package main

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// A probe is a request for a file no visitor asks for, such as /.env.
// Same filter as the Attacks row on the Web Traffic dashboard in the homelab repo.
// Alloy keeps US requests only, and hashes the address before Loki stores it.
const probeFilter = `{log_type=~"access|sandbox"}` +
	` | json host="RequestHost", path="RequestPath", visitor="[\"request_Cf-Connecting-Ip\"]", city="[\"request_Cf-Ipcity\"]", region="[\"request_Cf-Region-Code\"]", lat="[\"request_Cf-Iplatitude\"]", lon="[\"request_Cf-Iplongitude\"]"` +
	` | host!~"oidc.*|argocd-webhook.*"` +
	` | (path=~"(?i).*(/\\.[a-z_]|\\.(sql|bak|old|swp|save|tfstate|tfvars)|phpinfo|wp-config|xmlrpc\\.php|wlwmanifest).*" or (host!~"(kentjarrett|mattjarrett)\\.com" and path=~"(?i).*(\\.php|/wp-).*"))` +
	` | path!~"/\\.(well-known|ghost)/.*"`

// Sorts each probe into one kind of file by its path, first match wins.
// The kinds are fixed words from this template, so no text a scanner typed reaches the page.
const probeKind = ` | label_format kind="{{ $p := lower .path }}` +
	`{{ if contains \"/.env\" $p }}env` +
	`{{ else if contains \"/.git\" $p }}git` +
	`{{ else if or (contains \"/.aws\" $p) (contains \"/.ssh\" $p) (contains \"gcloud\" $p) (contains \"credentials\" $p) (contains \"/.docker\" $p) (contains \".npmrc\" $p) (contains \".netrc\" $p) (contains \".pypirc\" $p) (contains \".boto\" $p) (contains \"htpasswd\" $p) }}keys` +
	`{{ else if or (contains \"wp-\" $p) (contains \"wlwmanifest\" $p) (contains \"xmlrpc\" $p) }}wordpress` +
	`{{ else if or (contains \".sql\" $p) (contains \".bak\" $p) (contains \".old\" $p) (contains \".swp\" $p) (contains \".save\" $p) (contains \"tfstate\" $p) (contains \"tfvars\" $p) }}backup` +
	`{{ else if contains \".php\" $p }}php` +
	`{{ else }}other{{ end }}"`

// Sorts each probe by the wall that answered it.
// gateway: 429 is the rate limit, and 503 is a route with no site behind it.
// web: every other answer comes from the site's own web server.
const probeWall = ` | json status="DownstreamStatus"` +
	` | label_format wall="{{ if or (eq .status \"429\") (eq .status \"503\") }}gateway{{ else }}web{{ end }}"`

// The page shows the last 30 days, in buckets of 12 hours.
const (
	attackSpan = 30 * 24 * time.Hour
	attackStep = 12 * time.Hour
)

type attackStats struct {
	Probes  int `json:"probes"`
	Sources int `json:"sources"`
	// Timestamps: the end of each bucket.
	// Series: probes per bucket, including the ones with no location.
	Timestamps []int64       `json:"timestamps"`
	Series     []int         `json:"series"`
	Places     []attackPlace `json:"places"`
	// Kinds: probes by the kind of file asked for, most asked first.
	Kinds []attackKind `json:"kinds"`
	// Walls: probes by the wall that answered them, busiest first.
	Walls []attackWall `json:"walls"`
}

type attackWall struct {
	Wall  string `json:"wall"`
	Count int    `json:"count"`
}

type attackKind struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type attackPlace struct {
	City   string  `json:"city"`
	Region string  `json:"region"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	// Each hit is a bucket index and the probes in that bucket, oldest first.
	Hits [][2]int `json:"hits"`
}

func (s *server) attacks(ctx context.Context) (attackStats, error) {
	out := attackStats{Places: []attackPlace{}, Kinds: []attackKind{}, Walls: []attackWall{}}

	// Loki evaluates on multiples of the step, so the grid sits on them too.
	// The last bucket ends in the future, which keeps the newest probes in it.
	now := s.now()
	end := now.Truncate(attackStep).Add(attackStep)
	start := end.Add(-attackSpan + attackStep)
	index := map[int64]int{}
	for ts := start; !ts.After(end); ts = ts.Add(attackStep) {
		index[ts.Unix()] = len(out.Timestamps)
		out.Timestamps = append(out.Timestamps, min(ts.Unix(), now.Unix()))
	}
	out.Series = make([]int, len(out.Timestamps))

	byPlace, err := s.loki.queryRange(ctx,
		fmt.Sprintf(`sum by (city, region, lat, lon) (count_over_time(%s [%ds]))`, probeFilter, int(attackStep.Seconds())),
		start, end, attackStep)
	if err != nil {
		return out, err
	}

	// One city can arrive as several series with slightly different coordinates.
	type place struct {
		attackPlace
		buckets map[int]int
	}
	places := map[string]*place{}
	for _, sr := range byPlace {
		var p *place
		lat, lon, ok := coordinates(sr.labels["lat"], sr.labels["lon"])
		if city := sr.labels["city"]; ok && placeName.MatchString(city) && regionCode.MatchString(sr.labels["region"]) {
			id := city + "," + sr.labels["region"]
			if places[id] == nil {
				places[id] = &place{
					attackPlace: attackPlace{City: city, Region: sr.labels["region"], Lat: lat, Lon: lon},
					buckets:     map[int]int{},
				}
			}
			p = places[id]
		}
		for ts, v := range sr.points {
			i, ok := index[ts]
			if !ok {
				continue
			}
			out.Series[i] += int(v)
			out.Probes += int(v)
			if p != nil {
				p.buckets[i] += int(v)
			}
		}
	}
	for _, p := range places {
		for i, n := range p.buckets {
			p.Hits = append(p.Hits, [2]int{i, n})
		}
		sort.Slice(p.Hits, func(a, b int) bool { return p.Hits[a][0] < p.Hits[b][0] })
		out.Places = append(out.Places, p.attackPlace)
	}
	sort.Slice(out.Places, func(a, b int) bool {
		if out.Places[a].City != out.Places[b].City {
			return out.Places[a].City < out.Places[b].City
		}
		return out.Places[a].Region < out.Places[b].Region
	})

	sources, _, err := s.loki.scalar(ctx,
		fmt.Sprintf(`count(sum by (visitor) (count_over_time(%s [%ds])))`, probeFilter, int(attackSpan.Seconds())))
	out.Sources = int(sources)
	if err != nil {
		return out, err
	}

	kinds, err := s.loki.query(ctx,
		fmt.Sprintf(`sum by (kind) (count_over_time(%s%s [%ds]))`, probeFilter, probeKind, int(attackSpan.Seconds())))
	for _, k := range kinds {
		out.Kinds = append(out.Kinds, attackKind{Kind: k.labels["kind"], Count: int(k.value)})
	}
	sort.Slice(out.Kinds, func(a, b int) bool {
		if out.Kinds[a].Count != out.Kinds[b].Count {
			return out.Kinds[a].Count > out.Kinds[b].Count
		}
		return out.Kinds[a].Kind < out.Kinds[b].Kind
	})
	if err != nil {
		return out, err
	}

	walls, err := s.loki.query(ctx,
		fmt.Sprintf(`sum by (wall) (count_over_time(%s%s [%ds]))`, probeFilter, probeWall, int(attackSpan.Seconds())))
	for _, w := range walls {
		out.Walls = append(out.Walls, attackWall{Wall: w.labels["wall"], Count: int(w.value)})
	}
	sort.Slice(out.Walls, func(a, b int) bool {
		if out.Walls[a].Count != out.Walls[b].Count {
			return out.Walls[a].Count > out.Walls[b].Count
		}
		return out.Walls[a].Wall < out.Walls[b].Wall
	})
	return out, err
}

// The city and region come from request headers, and the page prints them.
// Only text shaped like a place name gets through, so a forged header cannot put its own words there.
// Anything else still counts in the totals, with no dot on the map.
var (
	placeName  = regexp.MustCompile(`^\p{L}[\p{L} .'-]{0,39}$`)
	regionCode = regexp.MustCompile(`^[A-Z]{2}$`)
)

// coordinates rounds to one decimal, about 11 km, so a dot marks a city and never a building.
func coordinates(latText, lonText string) (lat, lon float64, ok bool) {
	lat, latErr := strconv.ParseFloat(latText, 64)
	lon, lonErr := strconv.ParseFloat(lonText, 64)
	// The negated form also rejects NaN, which fails every comparison.
	if latErr != nil || lonErr != nil || !(math.Abs(lat) <= 90) || !(math.Abs(lon) <= 180) {
		return 0, 0, false
	}
	return math.Round(lat*10) / 10, math.Round(lon*10) / 10, true
}
