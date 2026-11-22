package lm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type countingDoer struct {
	calls atomic.Int32
}

func (d *countingDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"data":["up"]}`))),
	}, nil
}

func TestResponseCache_TTL(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cache := newResponseCache(time.Minute, 10)
	cache.now = func() time.Time { return now }

	cache.add("k", []byte("v"))
	body, ok := cache.get("k")
	require.True(t, ok)
	require.Equal(t, "v", string(body))

	now = now.Add(59 * time.Second)
	_, ok = cache.get("k")
	require.True(t, ok)

	now = now.Add(time.Second)
	_, ok = cache.get("k")
	require.False(t, ok)
}

func TestResponseCache_EvictsLeastRecentlyUsed(t *testing.T) {
	cache := newResponseCache(time.Minute, 2)
	cache.add("a", []byte("1"))
	cache.add("b", []byte("2"))
	_, ok := cache.get("a")
	require.True(t, ok)
	cache.add("c", []byte("3"))

	_, ok = cache.get("b")
	require.False(t, ok)
	_, ok = cache.get("a")
	require.True(t, ok)
	_, ok = cache.get("c")
	require.True(t, ok)
}

func TestClient_MetadataCacheExpiresAfterTTL(t *testing.T) {
	now := time.Unix(1700000000, 0)
	doer := &countingDoer{}
	client := NewClient(&Config{Account: "acme", BearerToken: "tok"}, doer, nil)
	client.shared.cache.now = func() time.Time { return now }

	for range 3 {
		_, err := client.MetricNames(context.Background(), &MetricNamesRequest{})
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, doer.calls.Load())

	now = now.Add(DefaultCacheTTLSeconds * time.Second)
	_, err := client.MetricNames(context.Background(), &MetricNamesRequest{})
	require.NoError(t, err)
	require.EqualValues(t, 2, doer.calls.Load())
}

func TestNewSharedStateUsesConfiguredLimits(t *testing.T) {
	state := NewSharedState(&Config{MaxConcurrentRequests: 3, CacheTTL: 5 * time.Second, CacheMaxEntries: 7})
	require.Equal(t, 3, cap(state.sem))
	require.Equal(t, 5*time.Second, state.cache.ttl)
	require.Equal(t, 7, state.cache.maxEntries)

	state = NewSharedState(&Config{})
	require.Equal(t, DefaultMaxConcurrentRequests, cap(state.sem))
	require.Equal(t, DefaultCacheTTLSeconds*time.Second, state.cache.ttl)
	require.Equal(t, DefaultCacheMaxEntries, state.cache.maxEntries)
}

func TestSharedStateFor(t *testing.T) {
	cfg := &Config{}
	httpClient := &http.Client{}
	require.Same(t, sharedStateFor(httpClient, cfg), sharedStateFor(httpClient, cfg))
	require.NotSame(t, sharedStateFor(httpClient, cfg), sharedStateFor(&http.Client{}, cfg))

	doer := &countingDoer{}
	require.NotSame(t, sharedStateFor(doer, cfg), sharedStateFor(doer, cfg))
}

func TestQueryRangeChunks(t *testing.T) {
	t.Run("aligns chunks to the step grid", func(t *testing.T) {
		require.Equal(t, []queryChunk{{0, 28}, {35, 63}, {70, 98}}, queryRangeChunks(0, 100, 7, 30))
	})

	t.Run("offset start keeps the grid of the start time", func(t *testing.T) {
		require.Equal(t, []queryChunk{{5, 35}, {45, 75}, {85, 95}}, queryRangeChunks(5, 100, 10, 30))
	})

	t.Run("step larger than chunk duration", func(t *testing.T) {
		require.Equal(t, []queryChunk{{0, 0}, {50, 50}, {100, 100}}, queryRangeChunks(0, 100, 50, 30))
	})

	t.Run("without step", func(t *testing.T) {
		require.Equal(t, []queryChunk{{0, 30}, {30, 60}, {60, 90}, {90, 100}}, queryRangeChunks(0, 100, 0, 30))
	})
}

func TestParseStep(t *testing.T) {
	require.Equal(t, 60.0, parseStep("60"))
	require.Equal(t, 0.5, parseStep("0.5"))
	require.Equal(t, 60.0, parseStep("1m"))
	require.Equal(t, 5400.0, parseStep("1h30m"))
	require.Equal(t, 0.0, parseStep(""))
	require.Equal(t, 0.0, parseStep("abc"))
	require.Equal(t, 0.0, parseStep("NaN"))
}
