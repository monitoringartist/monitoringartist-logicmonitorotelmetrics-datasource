package lm_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func gzipBytes(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(body)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func newRawLMServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func noCompressionBridge(srv *httptest.Server, cfg lm.Config, logger log.Logger) *lm.Bridge {
	cfg.Account = "test"
	cfg.RequestTimeout = 5 * time.Second
	client := testServerClient(srv, &http.Transport{DisableCompression: true})
	return lm.NewBridge(lm.NewClient(&cfg, client, logger), logger)
}

func TestHTTP_RequestHeadersAndPayloads(t *testing.T) {
	type seen struct {
		method, path, version, contentType, acceptEncoding string
		body                                               string
	}
	var mu sync.Mutex
	var requests []seen
	srv := newRawLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, seen{r.Method, r.URL.Path, r.Header.Get("X-Version"), r.Header.Get("Content-Type"), r.Header.Get("Accept-Encoding"), string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/santaba/rest/metrics/query", "/santaba/rest/metrics/query-range":
			_, _ = w.Write([]byte(`{"data":[{"metric":{"__name__":"up"},"values":[{"timestamp":1700000000,"value":"1"}]}]}`))
		default:
			_, _ = w.Write([]byte(`{"data":["up"]}`))
		}
	})
	bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, nil)

	_, err := bridge.QueryInstant(context.Background(), "up", "1700000000")
	require.NoError(t, err)
	_, err = bridge.QueryRange(context.Background(), "up", "1700000000", "1700003600", "60")
	require.NoError(t, err)
	_, err = bridge.QueryResource(context.Background(), &backend.CallResourceRequest{
		Path:    "api/v1/label/__name__/values",
		URL:     "api/v1/label/__name__/values?limit=1&match[]={__name__=~\"node_.*\"}",
		Method:  http.MethodGet,
		Headers: map[string][]string{"Accept-Encoding": {"gzip, deflate, br, zstd"}},
	})
	require.NoError(t, err)

	require.Len(t, requests, 3)
	for _, r := range requests {
		require.Equal(t, http.MethodPost, r.method)
		require.Equal(t, "3", r.version)
		require.Equal(t, "application/json", r.contentType)
		require.Empty(t, r.acceptEncoding, "incoming Accept-Encoding must never reach LogicMonitor")
	}
	require.Equal(t, "/santaba/rest/metrics/query", requests[0].path)
	require.JSONEq(t, `{"query":"up","time":1700000000}`, requests[0].body)
	require.Equal(t, "/santaba/rest/metrics/query-range", requests[1].path)
	require.JSONEq(t, `{"query":"up","start":"1700000000","end":"1700003600","step":"60"}`, requests[1].body)
	require.Equal(t, "/santaba/rest/metrics/metric-names", requests[2].path)
	require.JSONEq(t, `{"limit":1,"contains":"node_"}`, requests[2].body)
}

func TestHTTP_GzipResponsesAreDecoded(t *testing.T) {
	plain := []byte(`{"data":["__name__","job"]}`)
	for _, tc := range []struct {
		name   string
		header string
	}{
		{name: "Content-Encoding gzip", header: "gzip"},
		{name: "gzip magic bytes without Content-Encoding", header: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRawLMServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.header != "" {
					w.Header().Set("Content-Encoding", tc.header)
				}
				_, _ = w.Write(gzipBytes(t, plain))
			})
			bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, nil)

			resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{Path: "api/v1/labels", URL: "api/v1/labels"})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.JSONEq(t, `{"status":"success","data":["__name__","job"]}`, string(body))
		})
	}
}

func TestHTTP_GzipErrorResponseIsDecoded(t *testing.T) {
	srv := newRawLMServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(gzipBytes(t, []byte(`{"errorMessage":"Authentication failed"}`)))
	})
	bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, nil)

	resp, err := bridge.QueryInstant(context.Background(), "up", "")
	require.NoError(t, err)
	out := decodeBody(t, resp)
	require.Equal(t, "error", out["status"])
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, "unauthorized", out["errorType"])
	require.Equal(t, "LogicMonitor authentication failed (HTTP 401): Authentication failed", out["error"])
}

func TestHTTP_UnexpectedEncodingIsAnError(t *testing.T) {
	srv := newRawLMServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "zstd")
		_, _ = w.Write([]byte("opaque"))
	})
	bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, nil)

	resp, err := bridge.QueryInstant(context.Background(), "up", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	out := decodeBody(t, resp)
	require.Equal(t, "error", out["status"])
	require.Equal(t, "unavailable", out["errorType"])
	require.Contains(t, out["error"], `unexpected encoding type "zstd"`)
}

func TestHTTP_ResponseSizeCap(t *testing.T) {
	srv := newRawLMServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":["`))
		_, _ = w.Write(bytes.Repeat([]byte("a"), 16<<20))
		_, _ = w.Write([]byte(`"]}`))
	})
	bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, nil)

	resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{Path: "api/v1/labels", URL: "api/v1/labels"})
	require.NoError(t, err)
	out := decodeBody(t, resp)
	require.Equal(t, "error", out["status"])
	require.Contains(t, out["error"], "exceeds maximum size")
}

func TestHTTP_ServerErrorsAreRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := newRawLMServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"data":["job"]}`))
	})
	bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, nil)

	resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{Path: "api/v1/labels", URL: "api/v1/labels"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int32(2), attempts.Load())
}

func TestHTTP_RateLimitWithoutResetHintFailsFast(t *testing.T) {
	var attempts atomic.Int32
	srv := newRawLMServer(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("X-Rate-Limit-Remaining", "0")
		w.Header().Set("X-Rate-Limit-Window", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, nil)

	resp, err := bridge.QueryInstant(context.Background(), "up", "")
	require.NoError(t, err)
	out := decodeBody(t, resp)
	require.Contains(t, out["error"], "429")
	require.Equal(t, int32(1), attempts.Load())
}

func TestHTTP_ChunkedMetadataUsesParallelChunks(t *testing.T) {
	var mu sync.Mutex
	var starts []int64
	srv := newRawLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req lm.LabelValuesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		starts = append(starts, req.Start)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"data":["v` + strconv.FormatInt(req.Start, 10) + `","shared"]}`))
	})
	bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, nil)

	start := int64(1700000000)
	end := start + 60*86400
	resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{
		Path: "api/v1/label/job/values",
		URL:  "api/v1/label/job/values?start=" + strconv.FormatInt(start, 10) + "&end=" + strconv.FormatInt(end, 10),
	})
	require.NoError(t, err)
	out := decodeBody(t, resp)
	require.Equal(t, "success", out["status"])
	require.Len(t, out["data"], 4)
	require.Len(t, starts, 3)
}

func TestHTTP_QueryRangeExceedingMaxChunksIsRejected(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{})
	bridge := newHTTPBridge(srv, lm.Config{BearerToken: "tok"})

	start := int64(1000000000)
	end := start + 101*25*86400
	resp, err := bridge.QueryRange(context.Background(), "up", strconv.FormatInt(start, 10), strconv.FormatInt(end, 10), "3600")
	require.NoError(t, err)
	out := decodeBody(t, resp)
	require.Equal(t, "error", out["status"])
	require.Contains(t, out["error"], "maximum of 100 chunks")
	require.Empty(t, *captured)
}

func TestHTTP_SearchAPIIsUnavailable(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{})
	bridge := newHTTPBridge(srv, lm.Config{BearerToken: "tok"})

	resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{Path: "api/v1/search/metric_names", URL: "api/v1/search/metric_names"})
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	out := decodeBody(t, resp)
	require.Equal(t, "unavailable", out["errorType"])
	require.Empty(t, *captured)
}

func TestHTTP_RulesAPIIsNotSupported(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{})
	bridge := newHTTPBridge(srv, lm.Config{BearerToken: "tok"})

	for _, path := range []string{"api/v1/rules", "config/v1/rules"} {
		resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{Path: path, URL: path})
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	}
	require.Empty(t, *captured)
}

func TestHTTP_BuildInfoHasNoFeatures(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{})
	bridge := newHTTPBridge(srv, lm.Config{BearerToken: "tok"})

	resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{Path: "api/v1/status/buildinfo", URL: "api/v1/status/buildinfo"})
	require.NoError(t, err)
	out := decodeBody(t, resp)
	require.Equal(t, "success", out["status"])
	require.NotContains(t, out["data"], "features")
	require.Empty(t, *captured)
}

type debugRecorder struct {
	log.Logger
	mu   sync.Mutex
	msgs map[string][]any
}

func (l *debugRecorder) Debug(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.msgs == nil {
		l.msgs = map[string][]any{}
	}
	l.msgs[msg] = args
}

func (l *debugRecorder) Error(string, ...any) {}

func (l *debugRecorder) Level() log.Level { return log.Debug }

func TestHTTP_DebugLogsCappedDecodedBody(t *testing.T) {
	plain := []byte(`{"data":["` + strings.Repeat("x", 5000) + `"]}`)
	srv := newRawLMServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gzipBytes(t, plain))
	})
	logger := &debugRecorder{}
	bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, logger)

	_, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{Path: "api/v1/labels", URL: "api/v1/labels"})
	require.NoError(t, err)

	args := logger.msgs["LM HTTP response"]
	require.NotNil(t, args)
	fields := map[string]any{}
	for i := 0; i+1 < len(args); i += 2 {
		fields[args[i].(string)] = args[i+1]
	}
	require.Equal(t, len(plain), fields["body_bytes"])
	require.Equal(t, true, fields["body_truncated"])
	body := fields["body"].(string)
	require.True(t, strings.HasPrefix(body, `{"data":["xxx`), "logged body must be decoded, not gzip bytes")
	require.LessOrEqual(t, len(body), 4*1024+len("...(truncated)"))
}
