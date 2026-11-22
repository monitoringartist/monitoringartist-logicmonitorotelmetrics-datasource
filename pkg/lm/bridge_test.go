package lm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func TestBridge(t *testing.T) {
	mockClient := &mockHTTPClient{
		handler: func(req *http.Request) (*http.Response, error) {
			path := req.URL.Path
			switch path {
			case "/santaba/rest/metrics/query":
				return jsonResponse(http.StatusOK, map[string]interface{}{
					"data": []interface{}{
						map[string]interface{}{
							"metric": map[string]string{"__name__": "up", "job": "node"},
							"value":  map[string]interface{}{"timestamp": 1600000000, "value": "1"},
						},
					},
					"meta": map[string]interface{}{"resultType": "vector"},
				}), nil

			case "/santaba/rest/metrics/query-range":
				return jsonResponse(http.StatusOK, map[string]interface{}{
					"data": []interface{}{
						map[string]interface{}{
							"metric": map[string]string{"__name__": "up", "job": "node"},
							"values": []interface{}{
								map[string]interface{}{"timestamp": 1600000000, "value": "1"},
								map[string]interface{}{"timestamp": 1600000060, "value": "1"},
							},
						},
					},
					"meta": map[string]interface{}{"resultType": "matrix"},
				}), nil

			case "/santaba/rest/metrics/labels":
				return jsonResponse(http.StatusOK, map[string]interface{}{
					"data": []string{"__name__", "instance", "job"},
				}), nil

			case "/santaba/rest/metrics/label-values":
				return jsonResponse(http.StatusOK, map[string]interface{}{
					"data": []string{"node", "prometheus"},
				}), nil

			case "/santaba/rest/metrics/metric-names":
				return jsonResponse(http.StatusOK, map[string]interface{}{
					"data": []string{"up", "http_requests_total"},
				}), nil

			default:
				return jsonResponse(http.StatusNotFound, map[string]string{"message": "not found"}), nil
			}
		},
	}

	cfg := &lm.Config{
		Account:        "test",
		BearerToken:    "token",
		RequestTimeout: 10 * time.Second,
	}

	client := lm.NewClient(cfg, mockClient, nil)
	bridge := lm.NewBridge(client, nil)

	t.Run("QueryInstant", func(t *testing.T) {
		res, err := bridge.QueryInstant(context.Background(), "up", "1600000000")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)

		var resp lm.PromResponse
		err = json.Unmarshal(body, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		require.Equal(t, "vector", resp.Data.ResultType)
	})

	t.Run("QueryRange", func(t *testing.T) {
		res, err := bridge.QueryRange(context.Background(), "up", "1600000000", "1600000060", "60")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)

		var resp lm.PromResponse
		err = json.Unmarshal(body, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		require.Equal(t, "matrix", resp.Data.ResultType)
	})

	t.Run("QueryResource api/v1/labels", func(t *testing.T) {
		req := &backend.CallResourceRequest{
			Path:   "/api/v1/labels",
			Method: http.MethodGet,
			URL:    "/api/v1/labels",
		}
		res, err := bridge.QueryResource(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)

		var resp lm.PromMetaResponse
		err = json.Unmarshal(body, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		data := resp.Data.([]interface{})
		require.Contains(t, data, "__name__")
	})

	t.Run("QueryResource api/v1/label/__name__/values", func(t *testing.T) {
		req := &backend.CallResourceRequest{
			Path:   "/api/v1/label/__name__/values",
			Method: http.MethodGet,
			URL:    "/api/v1/label/__name__/values",
		}
		res, err := bridge.QueryResource(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)

		var resp lm.PromMetaResponse
		err = json.Unmarshal(body, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		data := resp.Data.([]interface{})
		require.Contains(t, data, "up")
	})

	t.Run("QueryResource api/v1/label/job/values", func(t *testing.T) {
		req := &backend.CallResourceRequest{
			Path:   "/api/v1/label/job/values",
			Method: http.MethodGet,
			URL:    "/api/v1/label/job/values",
		}
		res, err := bridge.QueryResource(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)

		var resp lm.PromMetaResponse
		err = json.Unmarshal(body, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		data := resp.Data.([]interface{})
		require.Contains(t, data, "node")
	})

	t.Run("QueryResource api/v1/series", func(t *testing.T) {
		req := &backend.CallResourceRequest{
			Path:   "/api/v1/series",
			Method: http.MethodGet,
			URL:    "/api/v1/series?match%5B%5D=up",
		}
		res, err := bridge.QueryResource(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)

		var resp lm.PromMetaResponse
		err = json.Unmarshal(body, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		series := resp.Data.([]interface{})
		require.Equal(t, []interface{}{map[string]interface{}{"__name__": "up", "job": "node"}}, series)
	})

	t.Run("QueryResource api/v1/status/buildinfo", func(t *testing.T) {
		req := &backend.CallResourceRequest{
			Path:   "/api/v1/status/buildinfo",
			Method: http.MethodGet,
			URL:    "/api/v1/status/buildinfo",
		}
		res, err := bridge.QueryResource(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)

		var resp lm.PromMetaResponse
		err = json.Unmarshal(body, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		data := resp.Data.(map[string]interface{})
		require.Equal(t, "lm-otel", data["version"])
		require.NotContains(t, data, "features")
	})

	t.Run("QueryResource api/v1/query_exemplars returns 404 unavailable", func(t *testing.T) {
		req := &backend.CallResourceRequest{
			Path:   "/api/v1/query_exemplars",
			Method: http.MethodGet,
			URL:    "/api/v1/query_exemplars",
		}
		res, err := bridge.QueryResource(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, res.StatusCode)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)

		var resp lm.PromMetaResponse
		require.NoError(t, json.Unmarshal(body, &resp))
		require.Equal(t, "error", resp.Status)
		require.Equal(t, "unavailable", resp.ErrorType)
	})

	t.Run("QueryResource forwards match[] label filters to LM discovery APIs", func(t *testing.T) {
		var lastBody []byte
		captureBridge := lm.NewBridge(lm.NewClient(&lm.Config{Account: "test", BearerToken: "tok"}, &mockHTTPClient{
			handler: func(req *http.Request) (*http.Response, error) {
				lastBody, _ = io.ReadAll(req.Body)
				return jsonResponse(http.StatusOK, map[string]interface{}{"data": []string{"orders"}}), nil
			},
		}, nil), nil)

		// Labels with non-__name__ matchers
		res, err := captureBridge.QueryResource(context.Background(), &backend.CallResourceRequest{
			Path:   "/api/v1/labels",
			Method: http.MethodGet,
			URL:    `/api/v1/labels?match%5B%5D=%7B__name__%3D%22up%22%2Cservice%3D%22orders%22%2Cregion%3D~%22us-.*%22%7D`,
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)

		var labelsReq lm.LabelsRequest
		require.NoError(t, json.Unmarshal(lastBody, &labelsReq))
		require.Equal(t, "up", labelsReq.Metric)
		require.Equal(t, map[string]lm.LabelMatcher{
			"service": {Op: "=", Values: []string{"orders"}},
			"region":  {Op: "=~", Values: []string{"us-.*"}},
		}, labelsReq.Filters)

		// Metric names (__name__ values) with equals filter
		res, err = captureBridge.QueryResource(context.Background(), &backend.CallResourceRequest{
			Path:   "/api/v1/label/__name__/values",
			Method: http.MethodGet,
			URL:    `/api/v1/label/__name__/values?match%5B%5D=%7Bservice%3D%22orders%22%2Cregion%3D~%22us-.*%22%7D`,
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode)

		var namesReq lm.MetricNamesRequest
		require.NoError(t, json.Unmarshal(lastBody, &namesReq))
		require.Equal(t, map[string][]string{"service": {"orders"}}, namesReq.Filters)
	})
}
