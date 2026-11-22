package lm_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

type capturedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   []byte
}

func newLMServer(t *testing.T, responses map[string]string) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedRequest{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body})
		mu.Unlock()
		resp, ok := responses[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorMessage":"not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

type rewriteTransport struct {
	host string
	base http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = t.host
	return t.base.RoundTrip(req)
}

func testServerClient(srv *httptest.Server, base http.RoundTripper) *http.Client {
	return &http.Client{Transport: rewriteTransport{host: strings.TrimPrefix(srv.URL, "http://"), base: base}}
}

func newHTTPBridge(srv *httptest.Server, cfg lm.Config) *lm.Bridge {
	cfg.Account = "test"
	cfg.RequestTimeout = 5 * time.Second
	return lm.NewBridge(lm.NewClient(&cfg, testServerClient(srv, http.DefaultTransport), nil), nil)
}

func decodeBody(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out map[string]interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func TestLMv1Authorization(t *testing.T) {
	body := []byte(`{"query":"up"}`)
	mac := hmac.New(sha256.New, []byte("key"))
	mac.Write([]byte("POST" + "1700000000000" + string(body) + "/metrics/query"))
	expected := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(mac.Sum(nil))))

	require.Equal(t, "LMv1 id:"+expected+":1700000000000", lm.LMv1Authorization("id", "key", "POST", "1700000000000", body, "/metrics/query"))
}

func TestHTTP_InstantQuerySignedRequest(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{
		"/santaba/rest/metrics/query": `{"data":[{"metric":{"__name__":"up","job":"a"},"values":[{"timestamp":1700000000,"value":"1"}]}]}`,
	})
	bridge := newHTTPBridge(srv, lm.Config{AccessID: "id", AccessKey: "key"})

	resp, err := bridge.QueryInstant(context.Background(), "up", "1700000000.5")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out := decodeBody(t, resp)
	require.Equal(t, "success", out["status"])
	data := out["data"].(map[string]interface{})
	require.Equal(t, "vector", data["resultType"])
	require.Len(t, data["result"], 1)

	require.Len(t, *captured, 1)
	req := (*captured)[0]
	require.Equal(t, http.MethodPost, req.Method)
	require.JSONEq(t, `{"query":"up","time":1700000000}`, string(req.Body))

	parts := strings.Split(strings.TrimPrefix(req.Auth, "LMv1 "), ":")
	require.Len(t, parts, 3)
	require.Equal(t, lm.LMv1Authorization("id", "key", "POST", parts[2], req.Body, "/metrics/query"), req.Auth)
}

func TestHTTP_RangeQueryLMEnvelope(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{
		"/santaba/rest/metrics/query-range": `{"data":[{"metric":{"__name__":"up"},"values":[{"timestamp":1700000000,"value":"1"},{"timestamp":1700000060,"value":"2"}]}],"meta":{"resultType":"matrix"}}`,
	})
	bridge := newHTTPBridge(srv, lm.Config{BearerToken: "tok"})

	resp, err := bridge.QueryRange(context.Background(), "up", "1700000000", "1700000060", "60")
	require.NoError(t, err)
	out := decodeBody(t, resp)
	require.Equal(t, "success", out["status"])
	require.Equal(t, "matrix", out["data"].(map[string]interface{})["resultType"])
	require.Equal(t, "Bearer tok", (*captured)[0].Auth)
}

func TestHTTP_ChunkedRangeQueryPrometheusEnvelopeWithMillis(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{
		"/santaba/rest/metrics/query-range": `{"data":{"resultType":"matrix","result":[{"metric":{"__name__":"up"},"values":[[1700000000000,"1"]]}]}}`,
	})
	bridge := newHTTPBridge(srv, lm.Config{BearerToken: "tok"})

	resp, err := bridge.QueryRange(context.Background(), "up", "1700000000", "1705000000", "3600")
	require.NoError(t, err)
	out := decodeBody(t, resp)
	require.Equal(t, "success", out["status"], out)
	result := out["data"].(map[string]interface{})["result"].([]interface{})
	require.Len(t, result, 1)
	values := result[0].(map[string]interface{})["values"].([]interface{})
	require.Len(t, values, 1)
	require.Equal(t, float64(1700000000), values[0].([]interface{})[0])
	require.Greater(t, len(*captured), 1)
}

func TestHTTP_ResourceEndpoints(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{
		"/santaba/rest/metrics/labels":       `{"data":["__name__","job"]}`,
		"/santaba/rest/metrics/label-values": `{"data":["a","b"]}`,
		"/santaba/rest/metrics/metric-names": `{"data":["up","go_goroutines"]}`,
	})
	bridge := newHTTPBridge(srv, lm.Config{BearerToken: "tok"})

	cases := []struct {
		url      string
		lmPath   string
		expected string
	}{
		{"api/v1/labels?match[]=up", "/santaba/rest/metrics/labels", `["__name__","job"]`},
		{"api/v1/label/job/values?start=1700000000.123&end=1700003600.456", "/santaba/rest/metrics/label-values", `["a","b"]`},
		{"api/v1/label/__name__/values", "/santaba/rest/metrics/metric-names", `["up","go_goroutines"]`},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			*captured = nil
			path := strings.SplitN(tc.url, "?", 2)[0]
			resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{Path: path, URL: tc.url, Method: http.MethodGet})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			out := decodeBody(t, resp)
			require.Equal(t, "success", out["status"])
			data, _ := json.Marshal(out["data"])
			require.JSONEq(t, tc.expected, string(data))
			require.Len(t, *captured, 1)
			require.Equal(t, tc.lmPath, (*captured)[0].Path)
		})
	}

	t.Run("label values forwards float start and end as seconds", func(t *testing.T) {
		*captured = nil
		_, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{Path: "api/v1/label/job/values", URL: "api/v1/label/job/values?start=1700000000.123&end=1700003600.456&match[]=up", Method: http.MethodGet})
		require.NoError(t, err)
		require.JSONEq(t, `{"key":"job","metric":"up","start":1700000000,"end":1700003600,"limit":200}`, string((*captured)[0].Body))
	})
}

func TestHTTP_ErrorsAreReturnedAsPrometheusErrors(t *testing.T) {
	srv, _ := newLMServer(t, map[string]string{})
	bridge := newHTTPBridge(srv, lm.Config{BearerToken: "tok"})

	resp, err := bridge.QueryInstant(context.Background(), "up", "")
	require.NoError(t, err)
	out := decodeBody(t, resp)
	require.Equal(t, "error", out["status"])
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "not_found", out["errorType"])
	require.Contains(t, out["error"], "HTTP 404")
}

func TestHTTP_MissingCredentialsDoesNotCallLM(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{})
	bridge := newHTTPBridge(srv, lm.Config{})

	resp, err := bridge.QueryInstant(context.Background(), "up", "")
	require.NoError(t, err)
	out := decodeBody(t, resp)
	require.Equal(t, "error", out["status"])
	require.Contains(t, out["error"], "credentials are not configured")
	require.Empty(t, *captured)
}
