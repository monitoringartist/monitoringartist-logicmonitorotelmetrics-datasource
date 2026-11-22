package lm_test

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func rateLimitHeader(remaining int, window string) http.Header {
	return http.Header{"X-Rate-Limit-Remaining": {strconv.Itoa(remaining)}, "X-Rate-Limit-Window": {window}}
}

func TestClient_429FailsFastWithClearError(t *testing.T) {
	var attempts atomic.Int32
	mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return rawResponse(http.StatusTooManyRequests, `{"errorMessage":"slow down"}`, rateLimitHeader(0, "60")), nil
	}}
	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok"}, mock, nil)

	_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.Equal(t, int32(1), attempts.Load())
	lmErr := lm.AsError(err)
	require.Equal(t, lm.ErrorTypeTooManyRequests, lmErr.Type)
	require.Equal(t, http.StatusTooManyRequests, lmErr.UpstreamStatus)
	require.Equal(t, "LogicMonitor returned 429; rate-limit headers do not indicate availability within 2s: slow down", lmErr.Message)
}

func TestClient_429HonorsConfiguredMaxRateLimitWait(t *testing.T) {
	var attempts atomic.Int32
	mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return rawResponse(http.StatusTooManyRequests, "", rateLimitHeader(0, "1")), nil
		}
		return rawResponse(http.StatusOK, `{"data":[]}`, nil), nil
	}}
	cfg := &lm.Config{Account: "test", BearerToken: "tok", MaxRateLimitWait: 500 * time.Millisecond}
	client := lm.NewClient(cfg, mock, nil)

	_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.Equal(t, int32(1), attempts.Load())
	require.Equal(t, "LogicMonitor returned 429; rate-limit headers do not indicate availability within 500ms", lm.AsError(err).Message)
}

func TestClient_429RetriesThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return rawResponse(http.StatusTooManyRequests, "", rateLimitHeader(0, "0.3")), nil
		}
		return rawResponse(http.StatusOK, `{"data":[]}`, rateLimitHeader(99, "60")), nil
	}}
	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok"}, mock, nil)

	began := time.Now()
	_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.NoError(t, err)
	require.Equal(t, int32(2), attempts.Load())
	require.GreaterOrEqual(t, time.Since(began), 300*time.Millisecond)
}

func TestClient_429ReportsAttemptsOnLastAttempt(t *testing.T) {
	var attempts atomic.Int32
	mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return rawResponse(http.StatusTooManyRequests, "", http.Header{"Retry-After": {"0"}}), nil
	}}
	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok", MaxRetryAttempts: 5}, mock, nil)

	_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.Equal(t, int32(5), attempts.Load())
	require.Equal(t, "LogicMonitor returned 429 after 5 attempts", lm.AsError(err).Message)
}

func TestClient_429WaitStopsOnCancellation(t *testing.T) {
	mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
		return rawResponse(http.StatusTooManyRequests, "", rateLimitHeader(0, "2")), nil
	}}
	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok"}, mock, nil)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	began := time.Now()
	_, err := client.Query(ctx, &lm.QueryRequest{Query: "up"})
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(began), time.Second)
}

func TestClient_ProactiveWaitStopsOnCancellation(t *testing.T) {
	var attempts atomic.Int32
	mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return rawResponse(http.StatusOK, `{"data":[]}`, rateLimitHeader(0, "60")), nil
	}}
	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok"}, mock, nil)
	_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	began := time.Now()
	_, err = client.Query(ctx, &lm.QueryRequest{Query: "up"})
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(began), time.Second)
	require.Equal(t, int32(1), attempts.Load())
}

func TestClient_ProactiveWaitIsBoundedByMaxRateLimitWait(t *testing.T) {
	var attempts atomic.Int32
	mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return rawResponse(http.StatusOK, `{"data":[]}`, rateLimitHeader(0, "60")), nil
	}}
	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok", MaxRateLimitWait: 300 * time.Millisecond}, mock, nil)
	_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.NoError(t, err)

	began := time.Now()
	_, err = client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	elapsed := time.Since(began)
	require.NoError(t, err)
	require.Equal(t, int32(2), attempts.Load())
	require.GreaterOrEqual(t, elapsed, 300*time.Millisecond)
	require.Less(t, elapsed, time.Second)
}

// fixedWindowServer simulates LogicMonitor's rate limiting: budget requests
// per window, reported in the X-Rate-Limit-* headers.
type fixedWindowServer struct {
	mu          sync.Mutex
	budget      int
	window      time.Duration
	windowStart time.Time
	used        int
	ok          int
	limited     int
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (s *fixedWindowServer) Do(*http.Request) (*http.Response, error) {
	inFlight := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		current := s.maxInFlight.Load()
		if inFlight <= current || s.maxInFlight.CompareAndSwap(current, inFlight) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.windowStart.IsZero() || now.Sub(s.windowStart) >= s.window {
		s.windowStart = now
		s.used = 0
	}
	windowHeader := strconv.FormatFloat(s.window.Seconds(), 'f', -1, 64)
	if s.used >= s.budget {
		s.limited++
		return rawResponse(http.StatusTooManyRequests, "", rateLimitHeader(0, windowHeader)), nil
	}
	s.used++
	s.ok++
	return rawResponse(http.StatusOK, `{"data":[]}`, rateLimitHeader(s.budget-s.used, windowHeader)), nil
}

func TestClient_ProactiveThrottlingWithParallelWorkers(t *testing.T) {
	server := &fixedWindowServer{budget: 3, window: time.Second}
	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok", RequestTimeout: 10 * time.Second}, server, nil)

	_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.NoError(t, err)

	start := int64(1700000000)
	end := start + 100*24*3600
	began := time.Now()
	_, err = client.QueryRange(context.Background(), &lm.QueryRequest{
		Query: "up",
		Start: strconv.FormatInt(start, 10),
		End:   strconv.FormatInt(end, 10),
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(began), 900*time.Millisecond)

	server.mu.Lock()
	defer server.mu.Unlock()
	require.Equal(t, 5, server.ok)
	require.Zero(t, server.limited, "requests beyond the remaining quota must wait instead of hitting 429")
}

func TestClient_QueryRangeWorkersLimitsParallelChunks(t *testing.T) {
	server := &fixedWindowServer{budget: 1000, window: time.Minute}
	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok", QueryRangeWorkers: 2}, server, nil)

	start := int64(1700000000)
	_, err := client.QueryRange(context.Background(), &lm.QueryRequest{
		Query: "up",
		Start: strconv.FormatInt(start, 10),
		End:   strconv.FormatInt(start+200*24*3600, 10),
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), server.maxInFlight.Load())
}

func TestClient_MaxRetryAttemptsAppliesToServerErrors(t *testing.T) {
	for _, maxAttempts := range []int{1, 4} {
		var attempts atomic.Int32
		mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
			attempts.Add(1)
			return rawResponse(http.StatusServiceUnavailable, "", http.Header{"Retry-After": {"0"}}), nil
		}}
		client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok", MaxRetryAttempts: maxAttempts}, mock, nil)

		_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
		require.Error(t, err)
		require.Equal(t, int32(maxAttempts), attempts.Load())
	}
}
