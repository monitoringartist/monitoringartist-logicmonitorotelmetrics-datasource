package lm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func TestClient_MetadataChunks(t *testing.T) {
	start := int64(1600000000)
	// 60 days duration -> requires 3 chunks (each 25 days max)
	end := start + 60*86400

	var callCount atomic.Int32
	mockClient := &mockHTTPClient{
		handler: func(req *http.Request) (*http.Response, error) {
			callCount.Add(1)
			body, _ := io.ReadAll(req.Body)
			var mnReq lm.MetricNamesRequest
			_ = json.Unmarshal(body, &mnReq)

			return jsonResponse(http.StatusOK, map[string]interface{}{
				"data": []string{
					"metric_" + strconv.FormatInt(mnReq.Start, 10),
					"common_metric",
				},
			}), nil
		},
	}

	cfg := &lm.Config{
		Account:        "test",
		BearerToken:    "token",
		RequestTimeout: 10 * time.Second,
	}

	client := lm.NewClient(cfg, mockClient, nil)
	resp, err := client.MetricNames(context.Background(), &lm.MetricNamesRequest{
		Start: start,
		End:   end,
		Limit: 100,
	})
	require.NoError(t, err)
	require.EqualValues(t, 3, callCount.Load())

	var apiResp struct {
		Data []string `json:"data"`
	}
	err = json.Unmarshal(resp, &apiResp)
	require.NoError(t, err)
	// Deduplicated: 3 unique chunk metrics + 1 common_metric = 4 items
	require.Len(t, apiResp.Data, 4)
	require.Contains(t, apiResp.Data, "common_metric")
}

type recordedMetadataRequest struct {
	Path  string
	Start int64
	End   int64
	Body  map[string]interface{}
}

func newMetadataRecorder(t *testing.T, respond func(path string, start, end int64) []string) (*lm.Client, func() []recordedMetadataRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []recordedMetadataRequest
	mockClient := &mockHTTPClient{
		handler: func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			var payload map[string]interface{}
			_ = json.Unmarshal(body, &payload)
			var timeRange struct {
				Start int64 `json:"start"`
				End   int64 `json:"end"`
			}
			_ = json.Unmarshal(body, &timeRange)

			mu.Lock()
			requests = append(requests, recordedMetadataRequest{Path: req.URL.Path, Start: timeRange.Start, End: timeRange.End, Body: payload})
			mu.Unlock()

			return jsonResponse(http.StatusOK, map[string]interface{}{
				"data": respond(req.URL.Path, timeRange.Start, timeRange.End),
			}), nil
		},
	}

	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "token", RequestTimeout: 10 * time.Second}, mockClient, nil)
	return client, func() []recordedMetadataRequest {
		mu.Lock()
		defer mu.Unlock()
		sorted := append([]recordedMetadataRequest(nil), requests...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
		return sorted
	}
}

func decodeMetadataData(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Data []string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp.Data
}

func TestClient_LabelsPassesTimeRange(t *testing.T) {
	start := int64(1600000000)
	end := start + 3600

	client, requests := newMetadataRecorder(t, func(string, int64, int64) []string {
		return []string{"job", "__name__"}
	})

	resp, err := client.Labels(context.Background(), &lm.LabelsRequest{Metric: "up", Start: start, End: end})
	require.NoError(t, err)

	recorded := requests()
	require.Len(t, recorded, 1)
	require.Equal(t, "/santaba/rest/metrics/labels", recorded[0].Path)
	require.Equal(t, start, recorded[0].Start)
	require.Equal(t, end, recorded[0].End)
	require.Equal(t, "up", recorded[0].Body["metric"])
	require.Equal(t, []string{"job", "__name__"}, decodeMetadataData(t, resp))
}

func TestClient_LabelsOmitsMissingTimeRange(t *testing.T) {
	client, requests := newMetadataRecorder(t, func(string, int64, int64) []string {
		return []string{"job"}
	})

	_, err := client.Labels(context.Background(), &lm.LabelsRequest{})
	require.NoError(t, err)

	recorded := requests()
	require.Len(t, recorded, 1)
	require.NotContains(t, recorded[0].Body, "start")
	require.NotContains(t, recorded[0].Body, "end")
}

func TestClient_LabelsChunksLongTimeRange(t *testing.T) {
	start := int64(1600000000)
	end := start + 60*86400

	client, requests := newMetadataRecorder(t, func(_ string, chunkStart, _ int64) []string {
		return []string{"zone_" + strconv.FormatInt(chunkStart, 10), "job", "__name__"}
	})

	resp, err := client.Labels(context.Background(), &lm.LabelsRequest{Start: start, End: end})
	require.NoError(t, err)

	recorded := requests()
	require.Len(t, recorded, 3)
	require.Equal(t, start, recorded[0].Start)
	require.Equal(t, end, recorded[len(recorded)-1].End)
	for i, r := range recorded {
		require.Equal(t, "/santaba/rest/metrics/labels", r.Path)
		require.LessOrEqual(t, r.End-r.Start, int64(25*86400))
		if i > 0 {
			require.Equal(t, recorded[i-1].End, r.Start)
		}
	}

	require.Equal(t, []string{
		"__name__",
		"job",
		"zone_" + strconv.FormatInt(recorded[0].Start, 10),
		"zone_" + strconv.FormatInt(recorded[1].Start, 10),
		"zone_" + strconv.FormatInt(recorded[2].Start, 10),
	}, decodeMetadataData(t, resp))
}

func TestClient_LabelValuesChunksDedupSortAndLimit(t *testing.T) {
	start := int64(1600000000)
	end := start + 60*86400

	client, requests := newMetadataRecorder(t, func(_ string, chunkStart, _ int64) []string {
		if chunkStart == start {
			return []string{"web", "api"}
		}
		return []string{"db", "api", "web"}
	})

	resp, err := client.LabelValues(context.Background(), &lm.LabelValuesRequest{Key: "job", Start: start, End: end, Limit: 2})
	require.NoError(t, err)

	recorded := requests()
	require.Len(t, recorded, 3)
	for _, r := range recorded {
		require.Equal(t, "/santaba/rest/metrics/label-values", r.Path)
		require.Equal(t, "job", r.Body["key"])
	}
	require.Equal(t, []string{"api", "db"}, decodeMetadataData(t, resp))
}

func TestClient_LabelsChunkErrorFailsRequest(t *testing.T) {
	start := int64(1600000000)
	end := start + 60*86400

	mockClient := &mockHTTPClient{
		handler: func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			var labelsReq lm.LabelsRequest
			_ = json.Unmarshal(body, &labelsReq)
			if labelsReq.Start != start {
				return jsonResponse(http.StatusBadRequest, map[string]string{"errorMessage": "bad range"}), nil
			}
			return jsonResponse(http.StatusOK, map[string]interface{}{"data": []string{"job"}}), nil
		},
	}

	client := lm.NewClient(&lm.Config{Account: "test", BearerToken: "token", RequestTimeout: 10 * time.Second}, mockClient, nil)
	_, err := client.Labels(context.Background(), &lm.LabelsRequest{Start: start, End: end})
	require.ErrorContains(t, err, "metadata chunk")
}

func TestBridge_LabelsEndpointPassesTimeRange(t *testing.T) {
	start := int64(1600000000)
	end := start + 60*86400

	client, requests := newMetadataRecorder(t, func(_ string, chunkStart, _ int64) []string {
		if chunkStart == start {
			return []string{"job", "instance"}
		}
		return []string{"__name__", "job"}
	})
	bridge := lm.NewBridge(client, nil)

	res, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{
		Path:   "/api/v1/labels",
		Method: http.MethodGet,
		URL:    "/api/v1/labels?match%5B%5D=up&start=" + strconv.FormatInt(start, 10) + "&end=" + strconv.FormatInt(end, 10),
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)

	recorded := requests()
	require.Len(t, recorded, 3)
	require.Equal(t, start, recorded[0].Start)
	require.Equal(t, end, recorded[2].End)
	require.Equal(t, "up", recorded[0].Body["metric"])

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	var resp lm.PromMetaResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Equal(t, "success", resp.Status)
	require.Equal(t, []interface{}{"__name__", "instance", "job"}, resp.Data)
}

func TestBridge_LabelValuesEndpointPassesTimeRange(t *testing.T) {
	start := int64(1600000000)
	end := start + 3600

	client, requests := newMetadataRecorder(t, func(string, int64, int64) []string {
		return []string{"api", "web"}
	})
	bridge := lm.NewBridge(client, nil)

	res, err := bridge.QueryResource(context.Background(), &backend.CallResourceRequest{
		Path:   "/api/v1/label/job/values",
		Method: http.MethodGet,
		URL:    "/api/v1/label/job/values?start=" + strconv.FormatInt(start, 10) + "&end=" + strconv.FormatInt(end, 10),
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)

	recorded := requests()
	require.Len(t, recorded, 1)
	require.Equal(t, start, recorded[0].Start)
	require.Equal(t, end, recorded[0].End)

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	var resp lm.PromMetaResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Equal(t, []interface{}{"api", "web"}, resp.Data)
}
