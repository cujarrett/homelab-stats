package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// cache holds one value per key with its own lock, so a burst of visitors
// triggers one Prometheus round trip per key, and one key's builder can read
// another key.
type cache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	now     func() time.Time
}

type cacheEntry struct {
	mu    sync.Mutex
	value any
	at    time.Time
	ok    bool
}

func newCache() *cache {
	return &cache{entries: map[string]*cacheEntry{}, now: time.Now}
}

func (c *cache) entry(key string) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		e = &cacheEntry{}
		c.entries[key] = e
	}
	return e
}

// get returns the cached value while it is younger than ttl, otherwise rebuilds it.
// A failed rebuild serves the last good value, because slightly old numbers beat
// an empty section when Prometheus restarts.
func (c *cache) get(ctx context.Context, key string, ttl time.Duration, build func(context.Context) (any, error)) (any, error) {
	e := c.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ok && c.now().Sub(e.at) < ttl {
		return e.value, nil
	}
	v, err := build(ctx)
	if err == nil {
		e.value, e.at, e.ok = v, c.now(), true
		return v, nil
	}
	if e.ok {
		slog.Warn("serving stale stats", "key", key, "age", c.now().Sub(e.at).String(), "err", err)
		return e.value, nil
	}
	return nil, err
}
