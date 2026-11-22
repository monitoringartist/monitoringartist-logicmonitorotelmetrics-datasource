package lm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"
)

const (
	maxQueryRangeChunks            = 100
	maxUpstreamResponseBytes int64 = 16 << 20 // 16 MiB
	maxLoggedBytes                 = 4 * 1024
	initialRetryBackoff            = 500 * time.Millisecond
	maxRetryWait                   = 10 * time.Second
)

// HTTPDoer executes HTTP requests.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is a LogicMonitor OpenTelemetry REST API client.
type Client struct {
	cfg        *Config
	httpClient HTTPDoer
	log        log.Logger
	shared     *SharedState
}

type QueryRequest struct {
	Query string `json:"query"`
	Time  int64  `json:"time,omitempty"`
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	Step  string `json:"step,omitempty"`
}

// LabelMatcher represents a label filter in LogicMonitor /metrics/labels and /metrics/label-values requests.
type LabelMatcher struct {
	Op     string   `json:"op"`
	Values []string `json:"values"`
}

type LabelsRequest struct {
	Metric  string                  `json:"metric,omitempty"`
	Start   int64                   `json:"start,omitempty"`
	End     int64                   `json:"end,omitempty"`
	Filters map[string]LabelMatcher `json:"filters,omitempty"`
}

type LabelValuesRequest struct {
	Key     string                  `json:"key"`
	Metric  string                  `json:"metric,omitempty"`
	Start   int64                   `json:"start,omitempty"`
	End     int64                   `json:"end,omitempty"`
	Limit   int                     `json:"limit,omitempty"`
	Filters map[string]LabelMatcher `json:"filters,omitempty"`
}

type MetricNamesRequest struct {
	Start    int64               `json:"start,omitempty"`
	End      int64               `json:"end,omitempty"`
	Limit    int                 `json:"limit,omitempty"`
	Contains string              `json:"contains,omitempty"`
	Filters  map[string][]string `json:"filters,omitempty"`
}

type lmAPIResponse struct {
	Data json.RawMessage        `json:"data"`
	Meta map[string]interface{} `json:"meta,omitempty"`
}

type lmMetricResult struct {
	Metric map[string]string `json:"metric,omitempty"`
	Values []lmValue         `json:"values,omitempty"`
	Value  *lmValue          `json:"value,omitempty"`
}

type lmValue struct {
	Timestamp float64 `json:"timestamp"`
	Value     string  `json:"value"`
}

// NewClient creates a new LogicMonitor client.
func NewClient(cfg *Config, httpClient HTTPDoer, plog log.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: cfg.RequestTimeout,
		}
	}
	return &Client{
		cfg:        cfg,
		httpClient: httpClient,
		log:        plog,
		shared:     sharedStateFor(httpClient, cfg),
	}
}

func (c *Client) doRequestWithContext(ctx context.Context, path string, payload interface{}) ([]byte, error) {
	if err := c.cfg.Validate(); err != nil {
		return nil, badDataError(err)
	}

	url := fmt.Sprintf("%s/santaba/rest%s", c.cfg.BaseURL(), path)

	var bodyBytes []byte
	var err error
	if payload != nil {
		bodyBytes, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request payload: %w", err)
		}
	}

	maxAttempts := c.cfg.maxRetryAttempts()
	for attempt := 1; ; attempt++ {
		if err := c.waitForRateLimit(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to create http request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Version", "3")

		epoch := strconv.FormatInt(time.Now().UnixNano()/int64(time.Millisecond), 10)

		switch c.cfg.EffectiveAuthType() {
		case AuthTypeBearer:
			req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
		case AuthTypeLMv1:
			req.Header.Set("Authorization", LMv1Authorization(c.cfg.AccessID, c.cfg.AccessKey, http.MethodPost, epoch, bodyBytes, path))
		}

		if err := c.acquire(ctx); err != nil {
			return nil, err
		}
		requestStart := time.Now()
		resp, err := c.httpClient.Do(req)
		if err != nil {
			c.release()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			lmErr := networkError(err)
			if attempt < maxAttempts && retryableNetworkError(err) {
				retry, waitErr := waitForRetry(ctx, jitter(retryBackoff(attempt)))
				if waitErr != nil {
					return nil, waitErr
				}
				if retry {
					continue
				}
			}
			return nil, lmErr
		}

		c.shared.limiter.observe(resp.Header)
		respBody, err := readResponse(resp)
		c.release()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, unavailableError(err)
		}

		if c.log != nil && (c.log.Level() == log.Debug || c.log.Level() == log.Trace) {
			c.log.Debug("LM HTTP response", "method", http.MethodPost, "url", url, "status_code", resp.StatusCode, "duration", time.Since(requestStart),
				"body_bytes", len(respBody), "body_truncated", len(respBody) > maxLoggedBytes, "body", truncate(string(respBody), maxLoggedBytes))
		}

		if resp.StatusCode == http.StatusOK {
			return respBody, nil
		}

		lmErr := statusError(resp.StatusCode, respBody)
		if tracker := HealthCheckTrackerFromContext(ctx); tracker != nil && lmErr.LMMessage != "" {
			tracker.SetErrorMessage(lmErr.LMMessage)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			retry, err := c.retryTooManyRequests(ctx, resp, lmErr, attempt, maxAttempts)
			if retry {
				continue
			}
			return nil, err
		}
		if attempt < maxAttempts {
			if delay, ok := retryDelay(resp, attempt); ok {
				retry, waitErr := waitForRetry(ctx, delay)
				if waitErr != nil {
					return nil, waitErr
				}
				if retry {
					continue
				}
			}
		}
		return nil, lmErr
	}
}

// retryTooManyRequests waits for a 429 response's Retry-After or rate-limit
// window and reports whether to retry. It fails fast when the headers do not
// indicate availability within the configured maximum wait.
func (c *Client) retryTooManyRequests(ctx context.Context, resp *http.Response, lmErr *Error, attempt, maxAttempts int) (bool, error) {
	if attempt >= maxAttempts {
		return false, withMessage(lmErr, fmt.Sprintf("LogicMonitor returned 429 after %d attempts", attempt))
	}
	maxWait := c.cfg.maxRateLimitWait()
	delay, ok := retryAfter(resp.Header.Get("Retry-After"))
	if ok {
		c.shared.limiter.block(delay)
		ok = delay <= maxRetryWait
		delay = jitter(delay)
	} else {
		delay, ok = rateLimitRetryDelay(resp, maxWait)
	}
	if ok {
		retry, err := waitForRetry(ctx, delay)
		if err != nil {
			return false, err
		}
		if retry {
			return true, nil
		}
	}
	return false, withMessage(lmErr, fmt.Sprintf("LogicMonitor returned 429; rate-limit headers do not indicate availability within %s", maxWait))
}

func readResponse(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	body, err := readLimited(resp.Body)
	if err != nil {
		return nil, err
	}
	return decodeResponseBody(resp.Header.Get("Content-Encoding"), body)
}

func readLimited(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxUpstreamResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if int64(len(body)) > maxUpstreamResponseBytes {
		return nil, fmt.Errorf("LogicMonitor response exceeds maximum size of %d bytes", maxUpstreamResponseBytes)
	}
	return body, nil
}

func decodeResponseBody(contentEncoding string, body []byte) ([]byte, error) {
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	isGzip := len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b
	if encoding == "gzip" || isGzip {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader for LogicMonitor response: %w", err)
		}
		defer func() { _ = zr.Close() }()
		decoded, err := readLimited(zr)
		if err != nil {
			return nil, fmt.Errorf("failed to decode gzip LogicMonitor response: %w", err)
		}
		return decoded, nil
	}
	if encoding != "" && encoding != "identity" {
		return nil, fmt.Errorf("unexpected encoding type %q in LogicMonitor response", contentEncoding)
	}
	return body, nil
}

// LMv1Authorization builds a LogicMonitor LMv1 Authorization header value.
func LMv1Authorization(accessID, accessKey, verb, epoch string, body []byte, resourcePath string) string {
	h := hmac.New(sha256.New, []byte(accessKey))
	h.Write([]byte(verb + epoch + string(body) + resourcePath))
	signature := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(h.Sum(nil))))
	return fmt.Sprintf("LMv1 %s:%s:%s", accessID, signature, epoch)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// Account returns the configured LogicMonitor account name.
func (c *Client) Account() string {
	return c.cfg.Account
}

// Timeout returns the effective per-call LogicMonitor API timeout.
func (c *Client) Timeout() time.Duration {
	if c.cfg.RequestTimeout > 0 {
		return c.cfg.RequestTimeout
	}
	return DefaultLMTimeoutSeconds * time.Second
}

func (c *Client) withTimeout(ctx context.Context, call func(context.Context) ([]byte, error)) ([]byte, error) {
	timeout := c.Timeout()
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := call(timeoutCtx)
	if err != nil && ctx.Err() == nil && errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
		return nil, &Error{
			Type:    ErrorTypeTimeout,
			Status:  http.StatusGatewayTimeout,
			Message: fmt.Sprintf("LogicMonitor API did not respond within %ds", int(timeout/time.Second)),
			Err:     err,
		}
	}
	return body, err
}

func (c *Client) Query(ctx context.Context, req *QueryRequest) ([]byte, error) {
	return c.withTimeout(ctx, func(ctx context.Context) ([]byte, error) {
		return c.doRequestWithContext(ctx, "/metrics/query", req)
	})
}

func (c *Client) QueryRange(ctx context.Context, req *QueryRequest) ([]byte, error) {
	return c.withTimeout(ctx, func(ctx context.Context) ([]byte, error) {
		return c.queryRange(ctx, req)
	})
}

func (c *Client) queryRange(ctx context.Context, req *QueryRequest) ([]byte, error) {
	if req.Start == "" || req.End == "" {
		return c.doRequestWithContext(ctx, "/metrics/query-range", req)
	}

	startVal, err1 := strconv.ParseFloat(req.Start, 64)
	endVal, err2 := strconv.ParseFloat(req.End, 64)
	if err1 != nil || err2 != nil {
		return c.doRequestWithContext(ctx, "/metrics/query-range", req)
	}

	if math.IsNaN(startVal) || math.IsInf(startVal, 0) ||
		math.IsNaN(endVal) || math.IsInf(endVal, 0) {
		return nil, badDataError(errors.New("query range start and end must be finite values"))
	}
	if endVal < startVal {
		return nil, badDataError(errors.New("query range end must not be before start"))
	}

	maxChunkDuration := float64(25 * 24 * 3600)
	duration := endVal - startVal
	maxRangeDuration := maxChunkDuration * maxQueryRangeChunks
	if duration > maxRangeDuration {
		return nil, badDataError(fmt.Errorf("query range exceeds maximum of %d chunks", maxQueryRangeChunks))
	}

	if duration <= maxChunkDuration {
		return c.doRequestWithContext(ctx, "/metrics/query-range", req)
	}

	chunks := queryRangeChunks(startVal, endVal, parseStep(req.Step), maxChunkDuration)
	if len(chunks) > maxQueryRangeChunks {
		return nil, badDataError(fmt.Errorf("query range exceeds maximum of %d chunks", maxQueryRangeChunks))
	}

	type chunkResult struct {
		metrics []lmMetricResult
		warning string
	}

	results, err := runChunks(ctx, c.cfg.queryRangeWorkers(), len(chunks), func(ctx context.Context, i int) (chunkResult, error) {
		chunk := chunks[i]
		subReq := &QueryRequest{
			Query: req.Query,
			Time:  req.Time,
			Start: strconv.FormatFloat(chunk.start, 'f', -1, 64),
			End:   strconv.FormatFloat(chunk.end, 'f', -1, 64),
			Step:  req.Step,
		}

		respBytes, err := c.doRequestWithContext(ctx, "/metrics/query-range", subReq)
		if err != nil {
			return chunkResult{}, fmt.Errorf("query-range chunk %d: %w", i+1, err)
		}

		data, _, warning, err := extractLMData(respBytes)
		if err != nil {
			return chunkResult{}, fmt.Errorf("decode LogicMonitor query-range response for chunk %d: %w", i+1, err)
		}

		chunkMetrics, err := decodeMetricResults(data)
		if err != nil {
			return chunkResult{}, fmt.Errorf("decode LogicMonitor query-range data for chunk %d: %w", i+1, err)
		}
		return chunkResult{metrics: chunkMetrics, warning: warning}, nil
	})
	if err != nil {
		return nil, err
	}

	var allMetrics []lmMetricResult
	var warnings []string
	seenWarnings := make(map[string]struct{})
	for _, res := range results {
		allMetrics = append(allMetrics, res.metrics...)
		if res.warning != "" {
			if _, ok := seenWarnings[res.warning]; !ok {
				seenWarnings[res.warning] = struct{}{}
				warnings = append(warnings, res.warning)
			}
		}
	}

	seriesMap := make(map[string]*lmMetricResult)
	for _, m := range allMetrics {
		key := metricKey(m.Metric)
		existing, ok := seriesMap[key]
		if !ok {
			mCopy := m
			mCopy.Values = make([]lmValue, len(m.Values))
			copy(mCopy.Values, m.Values)
			seriesMap[key] = &mCopy
		} else {
			existing.Values = append(existing.Values, m.Values...)
		}
	}

	keys := make([]string, 0, len(seriesMap))
	for key := range seriesMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	mergedMetrics := make([]lmMetricResult, 0, len(keys))
	for _, key := range keys {
		m := seriesMap[key]
		m.Values = sortAndDeduplicateValues(m.Values)
		mergedMetrics = append(mergedMetrics, *m)
	}

	mergedDataBytes, err := json.Marshal(mergedMetrics)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal merged query-range data: %w", err)
	}

	finalResp := lmAPIResponse{
		Data: mergedDataBytes,
	}
	if len(warnings) > 0 {
		finalResp.Meta = map[string]interface{}{
			"message": strings.Join(warnings, "; "),
		}
	}

	return json.Marshal(finalResp)
}

// runChunks runs call for chunk indexes [0, n) on at most workers
// goroutines and returns the results in chunk order. The first error cancels
// the remaining chunks.
func runChunks[T any](ctx context.Context, workers, n int, call func(ctx context.Context, i int) (T, error)) ([]T, error) {
	results := make([]T, n)
	jobs := make(chan int)
	g, gCtx := errgroup.WithContext(ctx)

	workerCount := min(max(workers, 1), n)
	for range workerCount {
		g.Go(func() error {
			for i := range jobs {
				result, err := call(gCtx, i)
				if err != nil {
					return err
				}
				results[i] = result
			}
			return nil
		})
	}

dispatch:
	for i := range n {
		select {
		case jobs <- i:
		case <-gCtx.Done():
			break dispatch
		}
	}
	close(jobs)

	if err := g.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

type queryChunk struct {
	start float64
	end   float64
}

// queryRangeChunks splits [start, end] into chunks of at most maxChunkDuration.
// With a positive step, every chunk starts on the start + k*step grid and the
// next chunk starts one step after the previous chunk ends, so each grid point
// is evaluated by exactly one chunk.
func queryRangeChunks(start, end, step, maxChunkDuration float64) []queryChunk {
	var chunks []queryChunk
	if step <= 0 {
		for chunkStart := start; chunkStart < end; chunkStart += maxChunkDuration {
			chunks = append(chunks, queryChunk{start: chunkStart, end: min(chunkStart+maxChunkDuration, end)})
		}
		return chunks
	}

	pointsPerChunk := math.Floor(maxChunkDuration/step) + 1
	totalPoints := math.Floor((end-start)/step) + 1
	for first := 0.0; first < totalPoints; first += pointsPerChunk {
		last := min(first+pointsPerChunk, totalPoints) - 1
		chunks = append(chunks, queryChunk{start: start + first*step, end: start + last*step})
	}
	return chunks
}

func parseStep(step string) float64 {
	step = strings.TrimSpace(step)
	if seconds, err := strconv.ParseFloat(step, 64); err == nil {
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return 0
		}
		return seconds
	}
	if d, err := model.ParseDuration(step); err == nil {
		return time.Duration(d).Seconds()
	}
	return 0
}

func metricKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(strings.ReplaceAll(k, "\x00", "\x01"))
		sb.WriteByte('\x00')
		sb.WriteString(strings.ReplaceAll(labels[k], "\x00", "\x01"))
		sb.WriteByte('\x00')
	}
	return sb.String()
}

func sortAndDeduplicateValues(values []lmValue) []lmValue {
	if len(values) <= 1 {
		return values
	}
	sort.Slice(values, func(i, j int) bool {
		return values[i].Timestamp < values[j].Timestamp
	})
	n := 1
	for i := 1; i < len(values); i++ {
		if values[i].Timestamp != values[i-1].Timestamp {
			values[n] = values[i]
			n++
		}
	}
	return values[:n]
}

func (c *Client) Labels(ctx context.Context, req *LabelsRequest) ([]byte, error) {
	return c.cached(ctx, "/metrics/labels", req, func(ctx context.Context) ([]byte, error) {
		return c.labels(ctx, req)
	})
}

func (c *Client) labels(ctx context.Context, req *LabelsRequest) ([]byte, error) {
	result, chunked, err := c.queryTimeChunkedStrings(
		ctx,
		"/metrics/labels",
		req.Start,
		req.End,
		0,
		func(start, end int64) interface{} {
			chunkRequest := *req
			chunkRequest.Start = start
			chunkRequest.End = end
			return &chunkRequest
		},
	)
	if chunked || err != nil {
		return result, err
	}

	return c.doRequestWithContext(ctx, "/metrics/labels", req)
}

func (c *Client) LabelValues(ctx context.Context, req *LabelValuesRequest) ([]byte, error) {
	reqCopy := *req
	if reqCopy.Limit <= 0 {
		reqCopy.Limit = 200
	}
	return c.cached(ctx, "/metrics/label-values", &reqCopy, func(ctx context.Context) ([]byte, error) {
		return c.labelValues(ctx, &reqCopy)
	})
}

func (c *Client) labelValues(ctx context.Context, req *LabelValuesRequest) ([]byte, error) {
	result, chunked, err := c.queryTimeChunkedStrings(
		ctx,
		"/metrics/label-values",
		req.Start,
		req.End,
		req.Limit,
		func(start, end int64) interface{} {
			chunkRequest := *req
			chunkRequest.Start = start
			chunkRequest.End = end
			return &chunkRequest
		},
	)
	if chunked || err != nil {
		return result, err
	}

	return c.doRequestWithContext(ctx, "/metrics/label-values", req)
}

func (c *Client) MetricNames(ctx context.Context, req *MetricNamesRequest) ([]byte, error) {
	reqCopy := *req
	if reqCopy.Limit <= 0 {
		reqCopy.Limit = 200
	}
	return c.cached(ctx, "/metrics/metric-names", &reqCopy, func(ctx context.Context) ([]byte, error) {
		return c.metricNames(ctx, &reqCopy)
	})
}

func (c *Client) metricNames(ctx context.Context, req *MetricNamesRequest) ([]byte, error) {
	result, chunked, err := c.queryTimeChunkedStrings(
		ctx,
		"/metrics/metric-names",
		req.Start,
		req.End,
		req.Limit,
		func(start, end int64) interface{} {
			chunkRequest := *req
			chunkRequest.Start = start
			chunkRequest.End = end
			return &chunkRequest
		},
	)
	if chunked || err != nil {
		return result, err
	}

	return c.doRequestWithContext(ctx, "/metrics/metric-names", req)
}

func retryBackoff(attempt int) time.Duration {
	return initialRetryBackoff << (attempt - 1)
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return min(d+rand.N(d/4+1), maxRetryWait)
}

// retryDelay reports how long to wait before retrying a failed response.
// Only 502, 503 and 504 responses are retried, and never for longer than maxRetryWait.
func retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	default:
		return 0, false
	}

	delay, ok := retryAfter(resp.Header.Get("Retry-After"))
	if !ok {
		delay = retryBackoff(attempt)
	}
	if delay > maxRetryWait {
		return 0, false
	}
	return jitter(delay), true
}

func retryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if t, err := http.ParseTime(value); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

// waitForRetry sleeps for delay and reports whether to retry. It does not
// wait when the delay would not leave time for another attempt before the
// context deadline, and returns the context error if the context ends first.
func waitForRetry(ctx context.Context, delay time.Duration) (bool, error) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
		return false, nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return true, nil
	}
}
