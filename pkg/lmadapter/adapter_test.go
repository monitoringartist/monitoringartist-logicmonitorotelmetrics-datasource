package lmadapter

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	sdkhttpclient "github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func TestExtendOptionsAddsLogicMonitorMiddleware(t *testing.T) {
	settings := backend.DataSourceInstanceSettings{
		URL:      "http://prometheus.invalid",
		JSONData: []byte(`{"account":"test","authType":"bearer"}`),
		DecryptedSecureJSONData: map[string]string{
			"token": "test-token",
		},
	}

	opts := &sdkhttpclient.Options{}
	err := ExtendOptions(context.Background(), settings, opts, log.DefaultLogger)
	require.NoError(t, err)
	require.Len(t, opts.Middlewares, 1)
}

func TestRoundTripperDoer(t *testing.T) {
	expected := &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
	}

	doer := roundTripperDoer{
		transport: sdkhttpclient.RoundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			return expected, nil
		}),
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
	require.NoError(t, err)

	resp, err := doer.Do(req)
	require.NoError(t, err)
	require.Same(t, expected, resp)
}

func TestExtendOptionsConcurrentRequestsAndSharedState(t *testing.T) {
	updated := time.Unix(1700000000, 0)
	settings := backend.DataSourceInstanceSettings{
		UID:      t.Name(),
		Updated:  updated,
		URL:      "http://prometheus.invalid",
		JSONData: []byte(`{"account":"test","authType":"bearer"}`),
		DecryptedSecureJSONData: map[string]string{
			"token": "test-token",
		},
	}

	var upstreamCalls atomic.Int32
	upstream := sdkhttpclient.RoundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     http.StatusText(http.StatusOK),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"data":["__name__","service_name"]}`)),
		}, nil
	})

	cfg, err := lm.NewConfigFromSettings(settings)
	require.NoError(t, err)
	shared := lm.NewSharedState(cfg)

	buildRT := func(s backend.DataSourceInstanceSettings, state *lm.SharedState) http.RoundTripper {
		opts := &sdkhttpclient.Options{}
		require.NoError(t, ExtendOptionsWithSharedState(state)(context.Background(), s, opts, log.DefaultLogger))
		return opts.Middlewares[0].CreateMiddleware(*opts, upstream)
	}

	rt1 := buildRT(settings, shared)
	rt2 := buildRT(settings, shared)

	// Concurrent POST requests with non-nil bodies should be race-free and share the metadata cache across rt1 and rt2.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rt := rt1
			if idx%2 == 1 {
				rt = rt2
			}
			req, err := http.NewRequest(http.MethodPost, "http://prometheus.invalid/api/v1/labels", strings.NewReader("start=1700000000&end=1700000300"))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
		}(i)
	}
	wg.Wait()
	require.EqualValues(t, 1, upstreamCalls.Load())

	// A new SharedState (e.g. after settings update) does not reuse the stale cache.
	rt3 := buildRT(settings, lm.NewSharedState(cfg))
	req, err := http.NewRequest(http.MethodPost, "http://prometheus.invalid/api/v1/labels", strings.NewReader("start=1700000000&end=1700000300"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := rt3.RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.EqualValues(t, 2, upstreamCalls.Load())
}

func TestExtendOptionsSkipsTLSVerifyForMockOnly(t *testing.T) {
	acmeSettings := backend.DataSourceInstanceSettings{
		JSONData: []byte(`{"account":"acme"}`),
	}
	otherSettings := backend.DataSourceInstanceSettings{
		JSONData: []byte(`{"account":"kkr"}`),
	}

	optsNoEnv := &sdkhttpclient.Options{}
	require.NoError(t, ExtendOptions(context.Background(), acmeSettings, optsNoEnv, log.DefaultLogger))
	require.Nil(t, optsNoEnv.TLS)

	t.Setenv("GF_PLUGIN_LM_MOCK_BASE_URL", "https://localhost:8443")

	optsAcme := &sdkhttpclient.Options{}
	require.NoError(t, ExtendOptions(context.Background(), acmeSettings, optsAcme, log.DefaultLogger))
	require.NotNil(t, optsAcme.TLS)
	require.True(t, optsAcme.TLS.InsecureSkipVerify)

	optsOther := &sdkhttpclient.Options{}
	require.NoError(t, ExtendOptions(context.Background(), otherSettings, optsOther, log.DefaultLogger))
	require.Nil(t, optsOther.TLS)
}
