package lm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

type mockHTTPClient struct {
	handler func(req *http.Request) (*http.Response, error)
}

func (m *mockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return m.handler(req)
}

func jsonResponse(status int, body interface{}) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body:          io.NopCloser(bytes.NewReader(b)),
		ContentLength: int64(len(b)),
	}
}

func TestClient_AuthHeaders(t *testing.T) {
	t.Run("uses Bearer token when configured", func(t *testing.T) {
		var capturedAuth string
		mockClient := &mockHTTPClient{
			handler: func(req *http.Request) (*http.Response, error) {
				capturedAuth = req.Header.Get("Authorization")
				require.Equal(t, "3", req.Header.Get("X-Version"))
				require.Equal(t, "application/json", req.Header.Get("Content-Type"))
				return jsonResponse(http.StatusOK, map[string]interface{}{"data": []interface{}{}}), nil
			},
		}

		cfg := &lm.Config{
			Account:        "test",
			BearerToken:    "myBearerToken123",
			RequestTimeout: 10 * time.Second,
		}

		client := lm.NewClient(cfg, mockClient, nil)
		_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
		require.NoError(t, err)
		require.Equal(t, "Bearer myBearerToken123", capturedAuth)
	})

	t.Run("uses LMv1 HMAC signature when accessID and accessKey configured", func(t *testing.T) {
		var capturedAuth string
		mockClient := &mockHTTPClient{
			handler: func(req *http.Request) (*http.Response, error) {
				capturedAuth = req.Header.Get("Authorization")
				return jsonResponse(http.StatusOK, map[string]interface{}{"data": []interface{}{}}), nil
			},
		}

		cfg := &lm.Config{
			Account:        "test",
			AccessID:       "testAccessID",
			AccessKey:      "testAccessKey",
			RequestTimeout: 10 * time.Second,
		}

		client := lm.NewClient(cfg, mockClient, nil)
		_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(capturedAuth, "LMv1 testAccessID:"))
		parts := strings.Split(capturedAuth, ":")
		require.Len(t, parts, 3)
	})
}

func TestClient_QueryRange_Chunking(t *testing.T) {
	start := int64(1600000000)
	// 60 days duration -> requires 3 chunks (each 25 days max)
	end := start + 60*86400

	var callCount atomic.Int32
	mockClient := &mockHTTPClient{
		handler: func(req *http.Request) (*http.Response, error) {
			callCount.Add(1)
			body, _ := io.ReadAll(req.Body)
			var qReq lm.QueryRequest
			_ = json.Unmarshal(body, &qReq)

			startFloat, _ := strconv.ParseFloat(qReq.Start, 64)
			return jsonResponse(http.StatusOK, map[string]interface{}{
				"data": []interface{}{
					map[string]interface{}{
						"metric": map[string]string{"__name__": "up", "instance": "server1"},
						"values": []interface{}{
							map[string]interface{}{"timestamp": startFloat, "value": "1"},
						},
					},
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
	resp, err := client.QueryRange(context.Background(), &lm.QueryRequest{
		Query: "up",
		Start: strconv.FormatInt(start, 10),
		End:   strconv.FormatInt(end, 10),
		Step:  "60",
	})
	require.NoError(t, err)
	require.EqualValues(t, 3, callCount.Load())

	var apiResp struct {
		Data []struct {
			Metric map[string]string `json:"metric"`
			Values []struct {
				Timestamp float64 `json:"timestamp"`
				Value     string  `json:"value"`
			} `json:"values"`
		} `json:"data"`
	}
	err = json.Unmarshal(resp, &apiResp)
	require.NoError(t, err)
	require.Len(t, apiResp.Data, 1)
	require.Len(t, apiResp.Data[0].Values, 3)
}

func TestClient_RateLimitRetry(t *testing.T) {
	attempts := 0
	mockClient := &mockHTTPClient{
		handler: func(req *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				resp := jsonResponse(http.StatusTooManyRequests, map[string]string{"message": "rate limit"})
				resp.Header.Set("X-Rate-Limit-Remaining", "0")
				resp.Header.Set("X-Rate-Limit-Window", "0.1")
				return resp, nil
			}
			return jsonResponse(http.StatusOK, map[string]interface{}{"data": []string{"m1"}}), nil
		},
	}

	cfg := &lm.Config{
		Account:        "test",
		BearerToken:    "token",
		RequestTimeout: 10 * time.Second,
	}

	client := lm.NewClient(cfg, mockClient, nil)
	resp, err := client.Labels(context.Background(), &lm.LabelsRequest{})
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
	require.Contains(t, string(resp), "m1")
}

func TestClient_AlwaysUsesLogicMonitorHost(t *testing.T) {
	var urls []string
	mockClient := &mockHTTPClient{
		handler: func(req *http.Request) (*http.Response, error) {
			urls = append(urls, req.URL.String())
			return jsonResponse(http.StatusOK, map[string]interface{}{"data": []interface{}{}}), nil
		},
	}

	client := lm.NewClient(&lm.Config{Account: "acme", BearerToken: "tok"}, mockClient, nil)
	_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.NoError(t, err)
	require.Len(t, urls, 1)
	require.True(t, strings.HasPrefix(urls[0], "https://acme.logicmonitor.com/santaba/rest/"), urls[0])

	t.Setenv("GF_PLUGIN_LM_MOCK_BASE_URL", "http://localhost:8080")
	_, err = client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.NoError(t, err)
	require.Len(t, urls, 2)
	require.True(t, strings.HasPrefix(urls[1], "http://localhost:8080/santaba/rest/"), urls[1])

	client = lm.NewClient(&lm.Config{Account: "kkr", BearerToken: "tok"}, mockClient, nil)
	_, err = client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
	require.NoError(t, err)
	require.Len(t, urls, 3)
	require.True(t, strings.HasPrefix(urls[2], "https://kkr.logicmonitor.com/santaba/rest/"), urls[2])

	for _, account := range []string{"evil.com", "127.0.0.1", "acme:8080", "acme/path", "a@b", ""} {
		client := lm.NewClient(&lm.Config{Account: account, BearerToken: "tok"}, mockClient, nil)
		_, err := client.Query(context.Background(), &lm.QueryRequest{Query: "up"})
		require.ErrorContains(t, err, "invalid LogicMonitor account")
	}
	require.Len(t, urls, 3)
}
