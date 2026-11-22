package lm_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func newMockBridge(cfg lm.Config, handler func(req *http.Request) (*http.Response, error)) *lm.Bridge {
	if cfg.Account == "" {
		cfg.Account = "test"
	}
	if cfg.BearerToken == "" {
		cfg.BearerToken = "tok"
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	return lm.NewBridge(lm.NewClient(&cfg, &mockHTTPClient{handler: handler}, nil), nil)
}

func rawResponse(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func TestBridge_ErrorMapping(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantStatus   int
		wantType     string
		wantError    string
		wantAttempts int32
	}{
		{"400", http.StatusBadRequest, `{"errorMessage":"invalid  query\nsyntax"}`, http.StatusBadRequest, "bad_data", "LogicMonitor rejected the request (HTTP 400): invalid query syntax", 1},
		{"422", http.StatusUnprocessableEntity, `{"errmsg":"cannot execute"}`, http.StatusBadRequest, "bad_data", "LogicMonitor rejected the request (HTTP 422): cannot execute", 1},
		{"401", http.StatusUnauthorized, `{"errorMessage":"Authentication failed","errorCode":1401}`, http.StatusUnauthorized, "unauthorized", "LogicMonitor authentication failed (HTTP 401): Authentication failed", 1},
		{"403", http.StatusForbidden, `{"message":"forbidden"}`, http.StatusForbidden, "forbidden", "LogicMonitor denied access (HTTP 403): forbidden", 1},
		{"404", http.StatusNotFound, ``, http.StatusNotFound, "not_found", "LogicMonitor resource not found (HTTP 404)", 1},
		{"429", http.StatusTooManyRequests, `{"errorMessage":"slow down"}`, http.StatusTooManyRequests, "too_many_requests", "LogicMonitor returned 429 after 3 attempts: slow down", 3},
		{"500", http.StatusInternalServerError, `<html><body>stack trace secret</body></html>`, http.StatusBadGateway, "unavailable", "Unexpected response from LogicMonitor API (HTTP 500)", 1},
		{"502", http.StatusBadGateway, `bad gateway secret`, http.StatusBadGateway, "unavailable", "Unexpected response from LogicMonitor API (HTTP 502)", 3},
		{"503", http.StatusServiceUnavailable, `{"errorMessage":"maintenance"}`, http.StatusServiceUnavailable, "unavailable", "LogicMonitor API is unavailable (HTTP 503): maintenance", 3},
		{"504", http.StatusGatewayTimeout, ``, http.StatusGatewayTimeout, "timeout", "LogicMonitor API timed out (HTTP 504)", 3},
		{"long LM message is truncated", http.StatusBadRequest, `{"errorMessage":"` + strings.Repeat("x", 300) + `"}`, http.StatusBadRequest, "bad_data", "LogicMonitor rejected the request (HTTP 400): " + strings.Repeat("x", 200) + "…", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			bridge := newMockBridge(lm.Config{}, func(*http.Request) (*http.Response, error) {
				attempts.Add(1)
				return rawResponse(tc.status, tc.body, http.Header{"Retry-After": {"0"}}), nil
			})

			resp, err := bridge.QueryInstant(context.Background(), "up", "")
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, resp.StatusCode)
			out := decodeBody(t, resp)
			require.Equal(t, "error", out["status"])
			require.Equal(t, tc.wantType, out["errorType"])
			require.Equal(t, tc.wantError, out["error"])
			require.NotContains(t, out["error"], "secret")
			require.Equal(t, tc.wantAttempts, attempts.Load())
		})
	}
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

func TestBridge_TransportErrorMapping(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantStatus   int
		wantType     string
		wantError    string
		wantAttempts int32
	}{
		{
			name:         "connection refused is retried",
			err:          &url.Error{Op: "Post", URL: "https://test.logicmonitor.com", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}},
			wantStatus:   http.StatusServiceUnavailable,
			wantType:     "unavailable",
			wantError:    "Could not connect to LogicMonitor API: dial tcp: connection refused",
			wantAttempts: 3,
		},
		{
			name:         "network timeout",
			err:          &url.Error{Op: "Post", URL: "https://test.logicmonitor.com", Err: timeoutNetError{}},
			wantStatus:   http.StatusGatewayTimeout,
			wantType:     "timeout",
			wantError:    "LogicMonitor API request timed out: i/o timeout",
			wantAttempts: 3,
		},
		{
			name:         "unknown host is not retried",
			err:          &url.Error{Op: "Post", URL: "https://test.logicmonitor.com", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "test.logicmonitor.com", IsNotFound: true}}},
			wantStatus:   http.StatusServiceUnavailable,
			wantType:     "unavailable",
			wantError:    "Could not connect to LogicMonitor API: dial tcp: lookup test.logicmonitor.com: no such host",
			wantAttempts: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			bridge := newMockBridge(lm.Config{}, func(*http.Request) (*http.Response, error) {
				attempts.Add(1)
				return nil, tc.err
			})

			resp, err := bridge.QueryInstant(context.Background(), "up", "")
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, resp.StatusCode)
			out := decodeBody(t, resp)
			require.Equal(t, tc.wantType, out["errorType"])
			require.Equal(t, tc.wantError, out["error"])
			require.Equal(t, tc.wantAttempts, attempts.Load())
		})
	}
}

func TestBridge_PluginAndRequestErrorMapping(t *testing.T) {
	ok := func(body string) func(*http.Request) (*http.Response, error) {
		return func(*http.Request) (*http.Response, error) {
			return rawResponse(http.StatusOK, body, nil), nil
		}
	}
	tests := []struct {
		name       string
		cfg        lm.Config
		handler    func(*http.Request) (*http.Response, error)
		wantStatus int
		wantType   string
	}{
		{"translation error", lm.Config{}, ok(`{"data":"not-a-result"}`), http.StatusInternalServerError, "internal"},
		{"non-JSON success body", lm.Config{}, ok(`<html>`), http.StatusInternalServerError, "internal"},
		{"missing credentials", lm.Config{AuthType: lm.AuthTypeLMv1, AccessID: "id"}, ok(`{"data":[]}`), http.StatusBadRequest, "bad_data"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bridge := newMockBridge(tc.cfg, tc.handler)
			resp, err := bridge.QueryInstant(context.Background(), "up", "")
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, resp.StatusCode)
			out := decodeBody(t, resp)
			require.Equal(t, tc.wantType, out["errorType"])
		})
	}
}

func TestAsError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantType   string
		wantStatus int
		downstream bool
	}{
		{"wrapped LM error", errors.Join(errors.New("chunk 1"), &lm.Error{Type: lm.ErrorTypeForbidden, Status: 403}), lm.ErrorTypeForbidden, 403, true},
		{"deadline", context.DeadlineExceeded, lm.ErrorTypeTimeout, http.StatusGatewayTimeout, true},
		{"canceled", context.Canceled, lm.ErrorTypeCanceled, 499, true},
		{"plain error", errors.New("decode failed"), lm.ErrorTypeInternal, http.StatusInternalServerError, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := lm.AsError(tc.err)
			require.Equal(t, tc.wantType, e.Type)
			require.Equal(t, tc.wantStatus, e.Status)
			require.Equal(t, tc.downstream, e.Downstream())
		})
	}
}
