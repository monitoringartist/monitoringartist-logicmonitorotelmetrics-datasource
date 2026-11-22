package lm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func newSlowLMClient(t *testing.T, delay time.Duration, timeout time.Duration) (*lm.Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	cfg := &lm.Config{
		Account:        "test",
		BearerToken:    "token",
		RequestTimeout: timeout,
	}
	return lm.NewClient(cfg, testServerClient(srv, http.DefaultTransport), nil), &calls
}

func TestClient_TimeoutEnforced(t *testing.T) {
	client, _ := newSlowLMClient(t, 5*time.Second, time.Second)

	began := time.Now()
	_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.EqualError(t, err, "LogicMonitor API did not respond within 1s")
	require.Less(t, time.Since(began), 3*time.Second)
}

func TestClient_TimeoutNotTriggeredForFastResponses(t *testing.T) {
	client, _ := newSlowLMClient(t, 0, time.Second)

	_, err := client.Labels(context.Background(), &lm.LabelsRequest{})
	require.NoError(t, err)
}

func TestClient_ChunkedCallsShareOneDeadline(t *testing.T) {
	start := int64(1600000000)
	end := start + 300*86400

	t.Run("query-range", func(t *testing.T) {
		client, calls := newSlowLMClient(t, 600*time.Millisecond, time.Second)

		began := time.Now()
		_, err := client.QueryRange(context.Background(), &lm.QueryRequest{
			Query: "up",
			Start: strconv.FormatInt(start, 10),
			End:   strconv.FormatInt(end, 10),
			Step:  "60",
		})
		require.EqualError(t, err, "LogicMonitor API did not respond within 1s")
		require.Less(t, time.Since(began), 2500*time.Millisecond)
		require.Greater(t, calls.Load(), int32(10))
	})

	t.Run("metadata", func(t *testing.T) {
		client, calls := newSlowLMClient(t, 600*time.Millisecond, time.Second)

		began := time.Now()
		_, err := client.MetricNames(context.Background(), &lm.MetricNamesRequest{Start: start, End: end})
		require.EqualError(t, err, "LogicMonitor API did not respond within 1s")
		require.Less(t, time.Since(began), 2500*time.Millisecond)
		require.Greater(t, calls.Load(), int32(10))
	})
}

func TestClient_ParentCancellationIsNotReportedAsTimeout(t *testing.T) {
	client, _ := newSlowLMClient(t, 5*time.Second, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := client.Query(ctx, &lm.QueryRequest{Query: "up"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "did not respond within")
}
