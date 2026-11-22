package lm_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func TestClient_RetryAfterBehavior(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		header       http.Header
		timeout      time.Duration
		wantAttempts int32
		wantSuccess  bool
		minElapsed   time.Duration
		maxElapsed   time.Duration
	}{
		{
			name:         "429 waits for Retry-After seconds",
			status:       http.StatusTooManyRequests,
			header:       http.Header{"Retry-After": {"1"}},
			wantAttempts: 2,
			wantSuccess:  true,
			minElapsed:   time.Second,
			maxElapsed:   1500 * time.Millisecond,
		},
		{
			name:         "429 waits for Retry-After HTTP date",
			status:       http.StatusTooManyRequests,
			header:       http.Header{"Retry-After": {time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)}},
			wantAttempts: 2,
			wantSuccess:  true,
			maxElapsed:   2600 * time.Millisecond,
		},
		{
			name:         "429 waits for the X-Rate-Limit window",
			status:       http.StatusTooManyRequests,
			header:       http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"1"}},
			wantAttempts: 2,
			wantSuccess:  true,
			minElapsed:   time.Second,
			maxElapsed:   1500 * time.Millisecond,
		},
		{
			name:         "429 Retry-After takes precedence over the X-Rate-Limit window",
			status:       http.StatusTooManyRequests,
			header:       http.Header{"Retry-After": {"0"}, "X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"60"}},
			wantAttempts: 2,
			wantSuccess:  true,
			maxElapsed:   500 * time.Millisecond,
		},
		{
			name:         "429 without hints fails fast",
			status:       http.StatusTooManyRequests,
			wantAttempts: 1,
			maxElapsed:   500 * time.Millisecond,
		},
		{
			name:         "429 with remaining quota fails fast",
			status:       http.StatusTooManyRequests,
			header:       http.Header{"X-Rate-Limit-Remaining": {"5"}, "X-Rate-Limit-Window": {"1"}},
			wantAttempts: 1,
			maxElapsed:   500 * time.Millisecond,
		},
		{
			name:         "429 Retry-After above the 10s cap fails fast",
			status:       http.StatusTooManyRequests,
			header:       http.Header{"Retry-After": {"11"}},
			wantAttempts: 1,
			maxElapsed:   500 * time.Millisecond,
		},
		{
			name:         "429 X-Rate-Limit window above the max rate-limit wait fails fast",
			status:       http.StatusTooManyRequests,
			header:       http.Header{"X-Rate-Limit-Remaining": {"0"}, "X-Rate-Limit-Window": {"3"}},
			wantAttempts: 1,
			maxElapsed:   500 * time.Millisecond,
		},
		{
			name:         "429 Retry-After beyond the request deadline fails fast",
			status:       http.StatusTooManyRequests,
			header:       http.Header{"Retry-After": {"3"}},
			timeout:      2 * time.Second,
			wantAttempts: 1,
			maxElapsed:   500 * time.Millisecond,
		},
		{
			name:         "503 honors Retry-After",
			status:       http.StatusServiceUnavailable,
			header:       http.Header{"Retry-After": {"1"}},
			wantAttempts: 2,
			wantSuccess:  true,
			minElapsed:   time.Second,
			maxElapsed:   1500 * time.Millisecond,
		},
		{
			name:         "500 is not retried",
			status:       http.StatusInternalServerError,
			header:       http.Header{"Retry-After": {"0"}},
			wantAttempts: 1,
			maxElapsed:   500 * time.Millisecond,
		},
		{
			name:         "401 is not retried",
			status:       http.StatusUnauthorized,
			header:       http.Header{"Retry-After": {"0"}},
			wantAttempts: 1,
			maxElapsed:   500 * time.Millisecond,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
				if attempts.Add(1) == 1 {
					return rawResponse(tc.status, `{"errorMessage":"retry later"}`, tc.header.Clone()), nil
				}
				return rawResponse(http.StatusOK, `{"data":["m1"]}`, nil), nil
			}}
			timeout := tc.timeout
			if timeout == 0 {
				timeout = 10 * time.Second
			}
			client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok", RequestTimeout: timeout}, mock, nil)

			began := time.Now()
			_, err := client.Labels(context.Background(), &lm.LabelsRequest{})
			elapsed := time.Since(began)

			require.Equal(t, tc.wantAttempts, attempts.Load())
			if tc.wantSuccess {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Equal(t, tc.status, lm.AsError(err).UpstreamStatus)
			}
			require.GreaterOrEqual(t, elapsed, tc.minElapsed)
			require.Less(t, elapsed, tc.maxElapsed)
		})
	}
}

func TestClient_RetriesStopAfterThreeAttempts(t *testing.T) {
	var attempts atomic.Int32
	mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return rawResponse(http.StatusTooManyRequests, "", http.Header{"Retry-After": {"0"}}), nil
	}}
	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok"}, mock, nil)

	_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.Equal(t, lm.ErrorTypeTooManyRequests, lm.AsError(err).Type)
	require.Equal(t, int32(3), attempts.Load())
}

func TestClient_RetryWaitStopsOnCancellation(t *testing.T) {
	mock := &mockHTTPClient{handler: func(*http.Request) (*http.Response, error) {
		return rawResponse(http.StatusTooManyRequests, "", http.Header{"Retry-After": {"5"}}), nil
	}}
	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok", RequestTimeout: 30 * time.Second}, mock, nil)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	began := time.Now()
	_, err := client.Query(ctx, &lm.QueryRequest{Query: "up"})
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(began), time.Second)
}
