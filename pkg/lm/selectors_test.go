package lm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func resourceURL(path string, params url.Values) string {
	return path + "?" + params.Encode()
}

func TestHTTP_MetricNamesComplexSelectors(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{
		"/santaba/rest/metrics/metric-names": `{"data":["up","node_cpu","node_mem","go_goroutines"]}`,
	})

	cases := []struct {
		name        string
		matches     []string
		lmBody      string
		expectNames []string
	}{
		{"bare name", []string{`up`}, `{"limit":200,"contains":"up"}`, []string{"up"}},
		{"name equality with labels", []string{`{__name__="up",job="x"}`}, `{"limit":200,"contains":"up","filters":{"job":["x"]}}`, []string{"up"}},
		{"bare name with labels", []string{`node_cpu{job="x"}`}, `{"limit":200,"contains":"node_cpu","filters":{"job":["x"]}}`, []string{"node_cpu"}},
		{"contains regex", []string{`{__name__=~".*node.*"}`}, `{"limit":200,"contains":"node"}`, []string{"node_cpu", "node_mem"}},
		{"complex regex", []string{`{__name__=~"node_(cpu|mem)"}`}, `{"limit":200}`, []string{"node_cpu", "node_mem"}},
		{"case-insensitive regex", []string{`{__name__=~"(?i)UP"}`}, `{"limit":200}`, []string{"up"}},
		{"regex with not equal", []string{`{__name__=~"node_.*",__name__!="node_mem"}`}, `{"limit":200,"contains":"node_"}`, []string{"node_cpu"}},
		{"negative regex", []string{`{__name__!~"node.*"}`}, `{"limit":200}`, []string{"up", "go_goroutines"}},
		{"multiple selectors", []string{`up`, `go_goroutines`}, `{"limit":200}`, []string{"up", "go_goroutines"}},
		{"invalid selector", []string{`{__name__=~"up`}, `{"limit":200}`, []string{"up", "node_cpu", "node_mem", "go_goroutines"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			*captured = nil
			bridge := newHTTPBridge(srv, lm.Config{BearerToken: "tok"})
			path := "api/v1/label/__name__/values"
			resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{
				Path:   path,
				URL:    resourceURL(path, url.Values{"match[]": tc.matches}),
				Method: http.MethodGet,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			out := decodeBody(t, resp)
			require.Equal(t, "success", out["status"])
			names, _ := json.Marshal(out["data"])
			expected, _ := json.Marshal(tc.expectNames)
			require.JSONEq(t, string(expected), string(names))
			require.Len(t, *captured, 1)
			require.JSONEq(t, tc.lmBody, string((*captured)[0].Body))
		})
	}
}

func TestHTTP_LabelsMetricFromSelector(t *testing.T) {
	srv, captured := newLMServer(t, map[string]string{
		"/santaba/rest/metrics/labels": `{"data":["__name__","job"]}`,
	})

	cases := []struct {
		match  string
		lmBody string
	}{
		{`foo`, `{"metric":"foo"}`},
		{`foo{job="x"}`, `{"metric":"foo","filters":{"job":{"op":"=","values":["x"]}}}`},
		{`{job="x",__name__="bar"}`, `{"metric":"bar","filters":{"job":{"op":"=","values":["x"]}}}`},
		{`{__name__=~"foo.*"}`, `{}`},
		{`{__name__!="foo"}`, `{}`},
		{`foo{job=`, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.match, func(t *testing.T) {
			*captured = nil
			bridge := newHTTPBridge(srv, lm.Config{BearerToken: "tok"})
			resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{
				Path:   "api/v1/labels",
				URL:    resourceURL("api/v1/labels", url.Values{"match[]": {tc.match}}),
				Method: http.MethodGet,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Len(t, *captured, 1)
			require.JSONEq(t, tc.lmBody, string((*captured)[0].Body))
		})
	}
}

func TestHTTP_SeriesReturnsLabelSets(t *testing.T) {
	var mu sync.Mutex
	var queries []lm.QueryRequest
	srv := newRawLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var q lm.QueryRequest
		_ = json.Unmarshal(body, &q)
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch q.Query {
		case `up{job="a"}`:
			_, _ = w.Write([]byte(`{"data":[{"metric":{"__name__":"up","job":"a","instance":"1"},"value":{"timestamp":1700000000,"value":"1"}},{"metric":{"__name__":"up","job":"a","instance":"2"},"value":{"timestamp":1700000000,"value":"1"}}]}`))
		case `{__name__=~"up|go_.*"}`:
			_, _ = w.Write([]byte(`{"data":[{"metric":{"__name__":"up","job":"a","instance":"1"},"value":{"timestamp":1700000000,"value":"1"}},{"metric":{"__name__":"go_goroutines","job":"b"},"value":{"timestamp":1700000000,"value":"5"}}],"meta":{"resultType":"vector"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":[]}`))
		}
	})
	bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, nil)

	query := func(params url.Values) map[string]interface{} {
		resp, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{
			Path:   "api/v1/series",
			URL:    resourceURL("api/v1/series", params),
			Method: http.MethodGet,
		})
		require.NoError(t, err)
		return decodeBody(t, resp)
	}

	t.Run("deduplicates label sets across selectors", func(t *testing.T) {
		queries = nil
		out := query(url.Values{"match[]": {`up{job="a"}`, `{__name__=~"up|go_.*"}`}, "start": {"1699990000"}, "end": {"1700000000"}})
		require.Equal(t, "success", out["status"])
		data, _ := json.Marshal(out["data"])
		require.JSONEq(t, `[
			{"__name__":"up","job":"a","instance":"1"},
			{"__name__":"up","job":"a","instance":"2"},
			{"__name__":"go_goroutines","job":"b"}
		]`, string(data))
		require.Equal(t, []lm.QueryRequest{
			{Query: `up{job="a"}`, Time: 1700000000},
			{Query: `{__name__=~"up|go_.*"}`, Time: 1700000000},
		}, queries)
	})

	t.Run("respects limit", func(t *testing.T) {
		queries = nil
		out := query(url.Values{"match[]": {`up{job="a"}`, `{__name__=~"up|go_.*"}`}, "end": {"1700000000"}, "limit": {"1"}})
		data, _ := json.Marshal(out["data"])
		require.JSONEq(t, `[{"__name__":"up","job":"a","instance":"1"}]`, string(data))
		require.Len(t, queries, 1)
	})

	t.Run("defaults time to now", func(t *testing.T) {
		queries = nil
		before := time.Now().Unix()
		out := query(url.Values{"match[]": {`up{job="a"}`}})
		require.Equal(t, "success", out["status"])
		require.Len(t, queries, 1)
		require.GreaterOrEqual(t, queries[0].Time, before)
		require.LessOrEqual(t, queries[0].Time, time.Now().Unix())
	})

	t.Run("requires match", func(t *testing.T) {
		queries = nil
		out := query(url.Values{})
		require.Equal(t, "error", out["status"])
		require.Equal(t, "bad_data", out["errorType"])
		require.Empty(t, queries)
	})
}

func TestHTTP_ChunkedRangeQuerySeriesOrderIsDeterministic(t *testing.T) {
	srv := newRawLMServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var q lm.QueryRequest
		_ = json.Unmarshal(body, &q)
		start, _ := strconv.ParseFloat(q.Start, 64)
		ts := strconv.FormatFloat(start, 'f', -1, 64)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[` +
			`{"metric":{"__name__":"up","job":"c"},"values":[{"timestamp":` + ts + `,"value":"1"}]},` +
			`{"metric":{"__name__":"up","job":"a"},"values":[{"timestamp":` + ts + `,"value":"1"}]},` +
			`{"metric":{"__name__":"up","job":"b"},"values":[{"timestamp":` + ts + `,"value":"1"}]},` +
			`{"metric":{"__name__":"up","instance":"x","job":"a"},"values":[{"timestamp":` + ts + `,"value":"1"}]}` +
			`]}`))
	})
	bridge := noCompressionBridge(srv, lm.Config{BearerToken: "tok"}, nil)

	for range 20 {
		resp, err := bridge.QueryRange(context.Background(), "up", "1700000000", "1705000000", "3600")
		require.NoError(t, err)
		out := decodeBody(t, resp)
		require.Equal(t, "success", out["status"], out)
		result := out["data"].(map[string]interface{})["result"].([]interface{})
		var labelSets []interface{}
		for _, r := range result {
			series := r.(map[string]interface{})
			labelSets = append(labelSets, series["metric"])
			require.Len(t, series["values"], 3)
		}
		require.Equal(t, []interface{}{
			map[string]interface{}{"__name__": "up", "instance": "x", "job": "a"},
			map[string]interface{}{"__name__": "up", "job": "a"},
			map[string]interface{}{"__name__": "up", "job": "b"},
			map[string]interface{}{"__name__": "up", "job": "c"},
		}, labelSets)
	}
}
