package lm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func newCacheTestClient(handler func(req *http.Request) (*http.Response, error)) *lm.Client {
	return lm.NewClient(&lm.Config{Account: "acme", BearerToken: "tok", RequestTimeout: 5 * time.Second}, &mockHTTPClient{handler: handler}, nil)
}

func TestClient_MetadataCacheHits(t *testing.T) {
	var calls atomic.Int32
	client := newCacheTestClient(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(http.StatusOK, map[string]interface{}{"data": []string{"up"}}), nil
	})
	ctx := context.Background()

	for range 2 {
		_, err := client.MetricNames(ctx, &lm.MetricNamesRequest{Contains: "up"})
		require.NoError(t, err)
		_, err = client.Labels(ctx, &lm.LabelsRequest{Metric: "up"})
		require.NoError(t, err)
		_, err = client.LabelValues(ctx, &lm.LabelValuesRequest{Key: "job", Metric: "up"})
		require.NoError(t, err)
	}
	require.EqualValues(t, 3, calls.Load())

	_, err := client.LabelValues(ctx, &lm.LabelValuesRequest{Key: "instance", Metric: "up"})
	require.NoError(t, err)
	require.EqualValues(t, 4, calls.Load())

	_, err = client.Query(ctx, &lm.QueryRequest{Query: "up"})
	require.NoError(t, err)
	_, err = client.Query(ctx, &lm.QueryRequest{Query: "up"})
	require.NoError(t, err)
	require.EqualValues(t, 6, calls.Load())
}

func TestClient_MetadataCacheIsPerAccount(t *testing.T) {
	var calls atomic.Int32
	httpClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(http.StatusOK, map[string]interface{}{"data": []string{req.URL.Host}}), nil
	})}

	a := lm.NewClient(&lm.Config{Account: "a", BearerToken: "tok"}, httpClient, nil)
	b := lm.NewClient(&lm.Config{Account: "b", BearerToken: "tok"}, httpClient, nil)
	bodyA, err := a.Labels(context.Background(), &lm.LabelsRequest{})
	require.NoError(t, err)
	bodyB, err := b.Labels(context.Background(), &lm.LabelsRequest{})
	require.NoError(t, err)
	require.Contains(t, string(bodyA), "a.logicmonitor.com")
	require.Contains(t, string(bodyB), "b.logicmonitor.com")
	require.EqualValues(t, 2, calls.Load())
}

func TestClient_MetadataCacheDoesNotCacheErrors(t *testing.T) {
	var calls atomic.Int32
	client := newCacheTestClient(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return jsonResponse(http.StatusInternalServerError, map[string]string{"errorMessage": "boom"}), nil
		}
		return jsonResponse(http.StatusOK, map[string]interface{}{"data": []string{"up"}}), nil
	})

	_, err := client.MetricNames(context.Background(), &lm.MetricNamesRequest{})
	require.Error(t, err)
	body, err := client.MetricNames(context.Background(), &lm.MetricNamesRequest{})
	require.NoError(t, err)
	require.Contains(t, string(body), "up")
	_, err = client.MetricNames(context.Background(), &lm.MetricNamesRequest{})
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
}

func TestClient_MetadataSingleflight(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	unblock := make(chan struct{})
	client := newCacheTestClient(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-unblock
		return jsonResponse(http.StatusOK, map[string]interface{}{"data": []string{"up"}}), nil
	})

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			body, err := client.LabelValues(context.Background(), &lm.LabelValuesRequest{Key: "job"})
			if err == nil && !json.Valid(body) {
				err = errors.New("invalid body")
			}
			errs <- err
		})
	}
	<-started
	time.Sleep(50 * time.Millisecond)
	close(unblock)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestClient_MetadataSingleflightSurvivesCanceledCaller(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	unblock := make(chan struct{})
	client := newCacheTestClient(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-unblock
		return jsonResponse(http.StatusOK, map[string]interface{}{"data": []string{"up"}}), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := client.Labels(ctx, &lm.LabelsRequest{})
		done <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	close(unblock)
	body, err := client.Labels(context.Background(), &lm.LabelsRequest{})
	require.NoError(t, err)
	require.Contains(t, string(body), "up")
	require.EqualValues(t, 1, calls.Load())
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type concurrencyProbe struct {
	inFlight atomic.Int32
	peak     atomic.Int32
	full     chan struct{}
	once     sync.Once
	unblock  chan struct{}
}

func newConcurrencyProbe() *concurrencyProbe {
	return &concurrencyProbe{full: make(chan struct{}), unblock: make(chan struct{})}
}

func (p *concurrencyProbe) handle(req *http.Request) (*http.Response, error) {
	n := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		peak := p.peak.Load()
		if n <= peak || p.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	if n == 16 {
		p.once.Do(func() { close(p.full) })
	}
	<-p.unblock
	return jsonResponse(http.StatusOK, map[string]interface{}{"data": []interface{}{}}), nil
}

func (p *concurrencyProbe) run(t *testing.T, clients []*lm.Client, callsPerClient int) {
	t.Helper()
	var wg sync.WaitGroup
	for _, client := range clients {
		for i := range callsPerClient {
			wg.Go(func() {
				_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up" + strconv.Itoa(i)})
				require.NoError(t, err)
			})
		}
	}
	select {
	case <-p.full:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for 16 concurrent requests")
	}
	time.Sleep(100 * time.Millisecond)
	require.EqualValues(t, 16, p.peak.Load())
	close(p.unblock)
	wg.Wait()
}

func TestClient_LimitsConcurrentRequests(t *testing.T) {
	probe := newConcurrencyProbe()
	client := lm.NewClient(&lm.Config{Account: "acme", BearerToken: "tok"}, &mockHTTPClient{handler: probe.handle}, nil)
	probe.run(t, []*lm.Client{client}, 40)
}

func TestClient_ConcurrencyLimitIsSharedByClientsOfOneInstance(t *testing.T) {
	probe := newConcurrencyProbe()
	httpClient := &http.Client{Transport: roundTripperFunc(probe.handle)}
	cfg := lm.Config{Account: "acme", BearerToken: "tok"}
	clients := []*lm.Client{
		lm.NewClient(&cfg, httpClient, nil),
		lm.NewClient(&cfg, httpClient, nil),
		lm.NewClient(&cfg, httpClient, nil),
	}
	probe.run(t, clients, 15)
}

func TestClient_QueryRangeChunksAreStepAligned(t *testing.T) {
	start := 1600000007.0
	step := 7 * 3600.0
	end := start + 60*86400

	var mu sync.Mutex
	var chunks [][2]float64
	client := newCacheTestClient(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		var q lm.QueryRequest
		_ = json.Unmarshal(body, &q)
		chunkStart, _ := strconv.ParseFloat(q.Start, 64)
		chunkEnd, _ := strconv.ParseFloat(q.End, 64)
		mu.Lock()
		chunks = append(chunks, [2]float64{chunkStart, chunkEnd})
		mu.Unlock()

		var values []map[string]interface{}
		for ts := chunkStart; ts <= chunkEnd; ts += step {
			values = append(values, map[string]interface{}{"timestamp": ts, "value": "1"})
		}
		return jsonResponse(http.StatusOK, map[string]interface{}{
			"data": []interface{}{map[string]interface{}{"metric": map[string]string{"__name__": "up"}, "values": values}},
		}), nil
	})

	resp, err := client.QueryRange(context.Background(), &lm.QueryRequest{
		Query: "up",
		Start: strconv.FormatFloat(start, 'f', -1, 64),
		End:   strconv.FormatFloat(end, 'f', -1, 64),
		Step:  strconv.FormatFloat(step, 'f', -1, 64),
	})
	require.NoError(t, err)
	require.Len(t, chunks, 3)

	mu.Lock()
	defer mu.Unlock()
	for _, chunk := range chunks {
		require.Zero(t, math.Mod(chunk[0]-start, step), chunk)
		require.Zero(t, math.Mod(chunk[1]-start, step), chunk)
		require.LessOrEqual(t, chunk[1]-chunk[0], 25*86400.0)
	}

	var merged struct {
		Data []struct {
			Values []struct {
				Timestamp float64 `json:"timestamp"`
			} `json:"values"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp, &merged))
	require.Len(t, merged.Data, 1)
	expectedPoints := int(math.Floor((end-start)/step)) + 1
	require.Len(t, merged.Data[0].Values, expectedPoints)
	for i, v := range merged.Data[0].Values {
		require.Equal(t, start+float64(i)*step, v.Timestamp)
	}

	total := 0
	for _, chunk := range chunks {
		total += int((chunk[1]-chunk[0])/step) + 1
	}
	require.Equal(t, expectedPoints, total)
}
