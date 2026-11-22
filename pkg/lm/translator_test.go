package lm_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func TestTranslateLMResponse(t *testing.T) {
	t.Run("matrix result type", func(t *testing.T) {
		lmJSON := []byte(`{
			"data": [
				{
					"metric": {"__name__": "http_requests_total", "code": "200"},
					"values": [
						{"timestamp": 1600000000, "value": "10"},
						{"timestamp": 1600000060, "value": "20"}
					]
				}
			],
			"meta": {
				"resultType": "matrix"
			}
		}`)

		translated, err := lm.TranslateLMResponse(lmJSON, "matrix")
		require.NoError(t, err)

		var resp lm.PromResponse
		err = json.Unmarshal(translated, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		require.Equal(t, "matrix", resp.Data.ResultType)

		resultSlice, ok := resp.Data.Result.([]interface{})
		require.True(t, ok)
		require.Len(t, resultSlice, 1)

		series := resultSlice[0].(map[string]interface{})
		metric := series["metric"].(map[string]interface{})
		require.Equal(t, "http_requests_total", metric["__name__"])

		values := series["values"].([]interface{})
		require.Len(t, values, 2)
		firstVal := values[0].([]interface{})
		require.Equal(t, float64(1600000000), firstVal[0])
		require.Equal(t, "10", firstVal[1])
	})

	t.Run("vector result type", func(t *testing.T) {
		lmJSON := []byte(`{
			"data": [
				{
					"metric": {"__name__": "up", "instance": "server1"},
					"value": {"timestamp": 1600000000, "value": "1"}
				}
			],
			"meta": {
				"resultType": "vector"
			}
		}`)

		translated, err := lm.TranslateLMResponse(lmJSON, "vector")
		require.NoError(t, err)

		var resp lm.PromResponse
		err = json.Unmarshal(translated, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		require.Equal(t, "vector", resp.Data.ResultType)

		resultSlice, ok := resp.Data.Result.([]interface{})
		require.True(t, ok)
		require.Len(t, resultSlice, 1)

		item := resultSlice[0].(map[string]interface{})
		val := item["value"].([]interface{})
		require.Equal(t, float64(1600000000), val[0])
		require.Equal(t, "1", val[1])
	})

	t.Run("scalar result type", func(t *testing.T) {
		lmJSON := []byte(`{
			"data": [
				{
					"value": {"timestamp": 1600000000, "value": "42"}
				}
			],
			"meta": {
				"resultType": "scalar"
			}
		}`)

		translated, err := lm.TranslateLMResponse(lmJSON, "scalar")
		require.NoError(t, err)

		var resp lm.PromResponse
		err = json.Unmarshal(translated, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		require.Equal(t, "scalar", resp.Data.ResultType)

		val := resp.Data.Result.([]interface{})
		require.Equal(t, float64(1600000000), val[0])
		require.Equal(t, "42", val[1])
	})

	t.Run("empty response with message", func(t *testing.T) {
		lmJSON := []byte(`{
			"data": null,
			"meta": {
				"resultType": "matrix",
				"message": "No data available"
			}
		}`)

		translated, err := lm.TranslateLMResponse(lmJSON, "matrix")
		require.NoError(t, err)

		var resp lm.PromResponse
		err = json.Unmarshal(translated, &resp)
		require.NoError(t, err)
		require.Equal(t, "success", resp.Status)
		require.Contains(t, resp.Warnings, "No data available")
	})
}

func TestTranslateMetadataResponse(t *testing.T) {
	lmJSON := []byte(`{
		"data": ["foo", "bar", "baz"]
	}`)

	translated, err := lm.TranslateMetadataResponse(lmJSON)
	require.NoError(t, err)

	var resp lm.PromMetaResponse
	err = json.Unmarshal(translated, &resp)
	require.NoError(t, err)
	require.Equal(t, "success", resp.Status)

	items := resp.Data.([]interface{})
	require.Len(t, items, 3)
	require.Equal(t, "foo", items[0])
}

func TestTranslateSeriesResponse(t *testing.T) {
	lmJSON := []byte(`{
		"data": ["cpu_usage", "memory_usage"]
	}`)

	translated, err := lm.TranslateSeriesResponse(lmJSON)
	require.NoError(t, err)

	var resp lm.PromMetaResponse
	err = json.Unmarshal(translated, &resp)
	require.NoError(t, err)
	require.Equal(t, "success", resp.Status)

	items := resp.Data.([]interface{})
	require.Len(t, items, 2)
	first := items[0].(map[string]interface{})
	require.Equal(t, "cpu_usage", first["__name__"])
}

func TestTranslateMetricMetadataResponse(t *testing.T) {
	lmJSON := []byte(`{
		"data": ["up", "http_requests"]
	}`)

	translated, err := lm.TranslateMetricMetadataResponse(lmJSON)
	require.NoError(t, err)

	var resp struct {
		Status string                         `json:"status"`
		Data   map[string][]lm.MetricMetadata `json:"data"`
	}
	err = json.Unmarshal(translated, &resp)
	require.NoError(t, err)
	require.Equal(t, "success", resp.Status)
	require.Contains(t, resp.Data, "up")
	require.Equal(t, "gauge", resp.Data["up"][0].Type)
}

func TestTranslateLMResponse_DoesNotPassThroughPrometheusShapedBodies(t *testing.T) {
	body := []byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"__name__":"up"},"values":[[1700000000000,"1"]]}]},"extra":"ignored"}`)

	translated, err := lm.TranslateLMResponse(body, "matrix")
	require.NoError(t, err)
	require.JSONEq(t, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"__name__":"up"},"values":[[1700000000,"1"]]}]}}`, string(translated))
}

func TestTranslateMetadataResponses_DoNotPassThroughPrometheusShapedBodies(t *testing.T) {
	t.Run("metadata", func(t *testing.T) {
		translated, err := lm.TranslateMetadataResponse([]byte(`{"status":"success","data":["up"],"extra":"ignored"}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"status":"success","data":["up"]}`, string(translated))
	})

	t.Run("series", func(t *testing.T) {
		_, err := lm.TranslateSeriesResponse([]byte(`{"status":"success","data":[{"__name__":"up"}]}`))
		require.Error(t, err)
	})

	t.Run("metric metadata", func(t *testing.T) {
		translated, err := lm.TranslateMetricMetadataResponse([]byte(`{"status":"success","data":{"up":[{"type":"counter","help":"","unit":""}]}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"status":"success","data":{}}`, string(translated))
	})
}
