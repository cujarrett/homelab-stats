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
	mu         sync.Mutex
	value      any
	at         time.Time
	ok         bool
	refreshing bool
}

// refreshTimeout bounds a background rebuild, which has no visitor's request to inherit a deadline from.
const refreshTimeout = 30 * time.Second

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

func (c *cache) get(ctx context.Context, key string, ttl time.Duration, build func(context.Context) (any, error)) (any, error) {
	v, _, err := c.getAt(ctx, key, ttl, build)
	return v, err
}

// getAt returns the cached value and when it was built.
//   - Nothing cached yet: the caller waits for the first build.
//   - Older than ttl: the caller gets the old value now, and one rebuild runs in the background.
//     A cold 30-day query takes about 9 seconds, and no visitor should wait on it.
func (c *cache) getAt(ctx context.Context, key string, ttl time.Duration, build func(context.Context) (any, error)) (any, time.Time, error) {
	e := c.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ok {
		if c.now().Sub(e.at) >= ttl && !e.refreshing {
			e.refreshing = true
			go c.refresh(key, e, build)
		}
		return e.value, e.at, nil
	}
	v, err := build(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	e.value, e.at, e.ok = v, c.now(), true
	return v, e.at, nil
}

// refresh rebuilds one entry. A failed rebuild keeps the last good value,
// because slightly old numbers beat an empty section when Prometheus restarts.
func (c *cache) refresh(key string, e *cacheEntry, build func(context.Context) (any, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	v, err := build(ctx)

	e.mu.Lock()
	defer e.mu.Unlock()
	e.refreshing = false
	if err != nil {
		countStale(key)
		slog.Warn("serving stale stats", "key", key, "age", c.now().Sub(e.at).String(), "err", err)
		return
	}
	e.value, e.at = v, c.now()
}
