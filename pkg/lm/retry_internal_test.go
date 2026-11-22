package lm

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRetryDelay(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		header  http.Header
		attempt int
		retry   bool
		base    time.Duration
	}{
		{"429 is handled by the rate-limit policy", http.StatusTooManyRequests, http.Header{"Retry-After": {"1"}}, 1, false, 0},
		{"503 Retry-After at cap", http.StatusServiceUnavailable, http.Header{"Retry-After": {"10"}}, 1, true, 10 * time.Second},
		{"503 Retry-After above cap", http.StatusServiceUnavailable, http.Header{"Retry-After": {"11"}}, 1, false, 0},
		{"503 invalid Retry-After uses backoff", http.StatusServiceUnavailable, http.Header{"Retry-After": {"soon"}}, 1, true, 500 * time.Millisecond},
		{"502 backoff", http.StatusBadGateway, nil, 1, true, 500 * time.Millisecond},
		{"503 second attempt backoff", http.StatusServiceUnavailable, nil, 2, true, time.Second},
		{"504 Retry-After", http.StatusGatewayTimeout, http.Header{"Retry-After": {"4"}}, 1, true, 4 * time.Second},
		{"500 not retried", http.StatusInternalServerError, nil, 1, false, 0},
		{"400 not retried", http.StatusBadRequest, http.Header{"Retry-After": {"1"}}, 1, false, 0},
		{"401 not retried", http.StatusUnauthorized, nil, 1, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: tc.header}
			if resp.Header == nil {
				resp.Header = http.Header{}
			}
			delay, retry := retryDelay(resp, tc.attempt)
			require.Equal(t, tc.retry, retry)
			if !tc.retry {
				return
			}
			require.GreaterOrEqual(t, delay, tc.base)
			require.LessOrEqual(t, delay, min(tc.base+tc.base/4, maxRetryWait))
		})
	}
}

func TestJitterIsBoundedAndVaries(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 50 {
		d := jitter(time.Second)
		require.GreaterOrEqual(t, d, time.Second)
		require.LessOrEqual(t, d, 1250*time.Millisecond)
		seen[d] = true
	}
	require.Greater(t, len(seen), 1)
	require.Equal(t, maxRetryWait, jitter(maxRetryWait))
	require.Equal(t, time.Duration(0), jitter(0))
}

func TestRateLimitRetryDelay(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header http.Header
		retry  bool
		delay  time.Duration
	}{
		{"window within max wait", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"1"}}, true, time.Second},
		{"fractional window", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {" 0 "}, "X-Rate-Limit-Window": {"0.5"}}, true, 500 * time.Millisecond},
		{"window at max wait", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"2"}}, true, 2 * time.Second},
		{"window too long", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"60"}}, false, 0},
		{"non-zero remaining", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"5"}, "X-Rate-Limit-Window": {"1"}}, false, 0},
		{"missing headers", http.StatusTooManyRequests, http.Header{}, false, 0},
		{"missing window", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"0"}}, false, 0},
		{"invalid remaining", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"none"}, "X-Rate-Limit-Window": {"1"}}, false, 0},
		{"invalid window", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"soon"}}, false, 0},
		{"NaN window", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"NaN"}}, false, 0},
		{"infinite window", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"+Inf"}}, false, 0},
		{"zero window", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"0"}}, false, 0},
		{"negative window", http.StatusTooManyRequests, http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"-1"}}, false, 0},
		{"not a 429", http.StatusServiceUnavailable, http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"1"}}, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			delay, ok := rateLimitRetryDelay(&http.Response{StatusCode: tc.status, Header: tc.header}, 2*time.Second)
			require.Equal(t, tc.retry, ok)
			require.Equal(t, tc.delay, delay)
		})
	}

	_, ok := rateLimitRetryDelay(nil, 2*time.Second)
	require.False(t, ok)
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newRateLimiter()
	limiter.now = func() time.Time { return now }

	require.Zero(t, limiter.reserve())

	limiter.observe(http.Header{"X-Rate-Limit-Remaining": {"2"}, "X-Rate-Limit-Window": {"10"}})
	require.Zero(t, limiter.reserve())
	require.Zero(t, limiter.reserve())
	require.Equal(t, 10*time.Second, limiter.reserve())

	limiter.observe(http.Header{"X-Rate-Limit-Remaining": {"5"}, "X-Rate-Limit-Window": {"10"}})
	require.Equal(t, 10*time.Second, limiter.reserve(), "a stale response must not raise the remaining count")

	now = now.Add(4 * time.Second)
	require.Equal(t, 6*time.Second, limiter.reserve())

	now = now.Add(6 * time.Second)
	require.Zero(t, limiter.reserve())

	limiter.observe(http.Header{"X-Rate-Limit-Remaining": {"bad"}, "X-Rate-Limit-Window": {"10"}})
	require.Zero(t, limiter.reserve())

	limiter.block(3 * time.Second)
	require.Equal(t, 3*time.Second, limiter.reserve())
}
