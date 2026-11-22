package lm

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"
	"weak"

	"golang.org/x/sync/singleflight"
)

// SharedState is shared by every Client of one datasource instance.
type SharedState struct {
	sem     chan struct{}
	cache   *responseCache
	group   singleflight.Group
	limiter *rateLimiter
}

// NewSharedState creates a SharedState for a datasource instance.
func NewSharedState(cfg *Config) *SharedState {
	return &SharedState{
		sem:     make(chan struct{}, cfg.maxConcurrentRequests()),
		cache:   newResponseCache(cfg.cacheTTL(), cfg.cacheMaxEntries()),
		limiter: newRateLimiter(),
	}
}

type sharedStateProvider interface {
	SharedState() *SharedState
}

var (
	sharedStatesMu sync.Mutex
	sharedStates   = map[weak.Pointer[http.Client]]*SharedState{}
)

// sharedStateFor returns the state shared by all clients built on the same
// datasource instance (via sharedStateProvider) or the same *http.Client.
func sharedStateFor(doer HTTPDoer, cfg *Config) *SharedState {
	if provider, ok := doer.(sharedStateProvider); ok {
		if state := provider.SharedState(); state != nil {
			return state
		}
	}

	httpClient, ok := doer.(*http.Client)
	if !ok {
		return NewSharedState(cfg)
	}

	key := weak.Make(httpClient)
	sharedStatesMu.Lock()
	defer sharedStatesMu.Unlock()
	if state, ok := sharedStates[key]; ok {
		return state
	}
	state := NewSharedState(cfg)
	sharedStates[key] = state
	runtime.AddCleanup(httpClient, func(key weak.Pointer[http.Client]) {
		sharedStatesMu.Lock()
		delete(sharedStates, key)
		sharedStatesMu.Unlock()
	}, key)
	return state
}

func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.shared.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) release() {
	<-c.shared.sem
}

// cached serves path/payload responses from the shared TTL cache. Identical
// concurrent calls share one upstream call, and errors are never cached.
func (c *Client) cached(ctx context.Context, path string, payload interface{}, call func(context.Context) ([]byte, error)) ([]byte, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request payload: %w", err)
	}
	key := c.cfg.Account + "\x00" + path + "\x00" + string(payloadBytes)

	if body, ok := c.shared.cache.get(key); ok {
		return body, nil
	}

	results := c.shared.group.DoChan(key, func() (interface{}, error) {
		body, err := c.withTimeout(context.WithoutCancel(ctx), call)
		if err != nil {
			return nil, err
		}
		c.shared.cache.add(key, body)
		return body, nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-results:
		if result.Err != nil {
			return nil, result.Err
		}
		return result.Val.([]byte), nil
	}
}

type responseCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	order      *list.List
	entries    map[string]*list.Element
}

type cacheEntry struct {
	key     string
	body    []byte
	expires time.Time
}

func newResponseCache(ttl time.Duration, maxEntries int) *responseCache {
	return &responseCache{
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
		order:      list.New(),
		entries:    map[string]*list.Element{},
	}
}

func (rc *responseCache) get(key string) ([]byte, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	elem, ok := rc.entries[key]
	if !ok {
		return nil, false
	}
	entry := elem.Value.(*cacheEntry)
	if !rc.now().Before(entry.expires) {
		rc.order.Remove(elem)
		delete(rc.entries, key)
		return nil, false
	}
	rc.order.MoveToFront(elem)
	return entry.body, true
}

func (rc *responseCache) add(key string, body []byte) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	expires := rc.now().Add(rc.ttl)
	if elem, ok := rc.entries[key]; ok {
		entry := elem.Value.(*cacheEntry)
		entry.body = body
		entry.expires = expires
		rc.order.MoveToFront(elem)
		return
	}
	rc.entries[key] = rc.order.PushFront(&cacheEntry{key: key, body: body, expires: expires})
	for rc.order.Len() > rc.maxEntries {
		oldest := rc.order.Back()
		rc.order.Remove(oldest)
		delete(rc.entries, oldest.Value.(*cacheEntry).key)
	}
}
