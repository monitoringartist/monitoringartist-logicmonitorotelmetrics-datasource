package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func doMockReq(t *testing.T, handler http.Handler, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestMockServer_AuthAndErrors(t *testing.T) {
	srv := newMockServer().routes()

	rec := doMockReq(t, srv, "/santaba/rest/metrics/metric-names", "", `{}`)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = doMockReq(t, srv, "/santaba/rest/metrics/label-values", "Bearer test", `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMockServer_DiscoveryEndpoints(t *testing.T) {
	srv := newMockServer().routes()

	// metric-names with contains
	rec := doMockReq(t, srv, "/santaba/rest/metrics/metric-names", "Bearer test", `{"contains":"sin"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var namesResp struct {
		Data []string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &namesResp))
	require.Equal(t, []string{"mocked_sinus"}, namesResp.Data)

	// labels scoped to mocked_sinus
	rec = doMockReq(t, srv, "/santaba/rest/metrics/labels", "Bearer test", `{"metric":"mocked_sinus"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var labelsResp struct {
		Data []string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &labelsResp))
	require.Contains(t, labelsResp.Data, "service_instance_id")

	// label-values for service_instance_id on mocked_sinus
	rec = doMockReq(t, srv, "/santaba/rest/metrics/label-values", "Bearer test", `{"key":"service_instance_id","metric":"mocked_sinus"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var valuesResp struct {
		Data []string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &valuesResp))
	require.Equal(t, []string{"sinus-instance-1", "sinus-instance-2"}, valuesResp.Data)
}

func TestMockServer_SinusQueryRange(t *testing.T) {
	srv := newMockServer().routes()

	rec := doMockReq(t, srv, "/santaba/rest/metrics/query-range", "LMv1 id:sig:1", `{"query":"mocked_sinus","start":1700000000,"end":1700000300,"step":"60s"}`)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []lmSeriesResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 2)
	require.Equal(t, "sinus-instance-1", resp.Data[0].Metric["service_instance_id"])
	require.Equal(t, "sinus-instance-2", resp.Data[1].Metric["service_instance_id"])
	require.Len(t, resp.Data[0].Values, 6)

	// Verify values vary over time across both short (1h) and long (30d, 1h step) ranges.
	v0, err := strconv.ParseFloat(resp.Data[0].Values[0].Value, 64)
	require.NoError(t, err)
	v1, err := strconv.ParseFloat(resp.Data[0].Values[1].Value, 64)
	require.NoError(t, err)
	require.Greater(t, math.Abs(v1-v0), 1.0)

	longRec := doMockReq(t, srv, "/santaba/rest/metrics/query-range", "LMv1 id:sig:1", `{"query":"mocked_sinus","start":1700000000,"end":1702592000,"step":"1h"}`)
	require.Equal(t, http.StatusOK, longRec.Code)
	var longResp struct {
		Data []lmSeriesResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(longRec.Body.Bytes(), &longResp))
	require.Len(t, longResp.Data, 2)
	for _, s := range longResp.Data {
		minVal, maxVal := math.Inf(1), math.Inf(-1)
		for _, pt := range s.Values {
			v, err := strconv.ParseFloat(pt.Value, 64)
			require.NoError(t, err)
			minVal = min(minVal, v)
			maxVal = max(maxVal, v)
		}
		require.Greater(t, maxVal-minVal, 40.0)
	}
}

func TestMockServer_BridgeIntegration(t *testing.T) {
	ts := httptest.NewServer(newMockServer().routes())
	defer ts.Close()

	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		cloned := req.Clone(req.Context())
		cloned.URL.Scheme = "http"
		cloned.URL.Host = strings.TrimPrefix(ts.URL, "http://")
		return http.DefaultClient.Do(cloned)
	})

	cfg := &lm.Config{
		Account:        "acme",
		AuthType:       lm.AuthTypeBearer,
		BearerToken:    "local-dev-token",
		RequestTimeout: 5 * time.Second,
	}
	bridge := lm.NewBridge(lm.NewClient(cfg, doer, nil), nil)

	resp, err := bridge.QueryRange(context.Background(), "mocked_sinus", "1700000000", "1700000300", "60s")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var prom struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(buf.Bytes(), &prom))
	require.Equal(t, "success", prom.Status)
	require.Equal(t, "matrix", prom.Data.ResultType)
	require.Len(t, prom.Data.Result, 2)

	// Verify healthcheck instant query ("1+1") works end-to-end through the bridge.
	hcResp, err := bridge.QueryInstant(context.Background(), "1+1", "4")
	require.NoError(t, err)
	defer func() { _ = hcResp.Body.Close() }()
	require.Equal(t, http.StatusOK, hcResp.StatusCode)

	var hcProm struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []any  `json:"result"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(hcResp.Body).Decode(&hcProm))
	require.Equal(t, "success", hcProm.Status)
	require.Equal(t, "scalar", hcProm.Data.ResultType)
	require.Len(t, hcProm.Data.Result, 2)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}
