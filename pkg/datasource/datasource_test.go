package datasource

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	sdkhttpclient "github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/config"
	promlib "github.com/grafana/grafana-prometheus-datasource/pkg/promlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lmadapter"
)

var (
	_ backend.QueryDataHandler    = (*Datasource)(nil)
	_ backend.CallResourceHandler = (*Datasource)(nil)
	_ backend.CheckHealthHandler  = (*Datasource)(nil)
	_ backend.AdmissionHandler    = (*Datasource)(nil)
	_ instancemgmt.Instance       = (*Datasource)(nil)
)

func TestNewDatasource(t *testing.T) {
	inst, err := NewDatasource(context.Background(), backend.DataSourceInstanceSettings{})
	require.NoError(t, err)
	ds, ok := inst.(*Datasource)
	require.True(t, ok)
	require.NotNil(t, ds.Service)
}

func roundTripWithContextualMiddlewares(t *testing.T, ctx context.Context, body string) ([]byte, error) {
	t.Helper()
	middlewares := sdkhttpclient.ContextualMiddlewareFromContext(ctx)
	require.Len(t, middlewares, 1)
	named, ok := middlewares[0].(sdkhttpclient.MiddlewareName)
	require.True(t, ok)
	assert.Equal(t, sdkhttpclient.ResponseLimitMiddlewareName, named.MiddlewareName())

	next := sdkhttpclient.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	rt := middlewares[0].CreateMiddleware(sdkhttpclient.Options{}, next)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com", nil)
	require.NoError(t, err)
	res, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer func() {
		_ = res.Body.Close()
	}()
	return io.ReadAll(res.Body)
}

func TestContextualMiddlewares_ResponseLimitFromGrafanaConfig(t *testing.T) {
	t.Setenv("GF_DATAPROXY_RESPONSE_LIMIT", "")
	ctx := config.WithGrafanaConfig(context.Background(), config.NewGrafanaCfg(map[string]string{
		config.ResponseLimit: "5",
	}))

	ctx = (&Datasource{}).contextualMiddlewares(ctx)

	_, err := roundTripWithContextualMiddlewares(t, ctx, "more than five bytes")
	require.ErrorIs(t, err, sdkhttpclient.ErrResponseBodyTooLarge)
}

func TestContextualMiddlewares_NoLimitWithoutGrafanaConfig(t *testing.T) {
	t.Setenv("GF_DATAPROXY_RESPONSE_LIMIT", "")

	ctx := (&Datasource{}).contextualMiddlewares(context.Background())

	got, err := roundTripWithContextualMiddlewares(t, ctx, "more than five bytes")
	require.NoError(t, err)
	assert.Equal(t, "more than five bytes", string(got))
}

func TestContextualMiddlewares_AppendsToExisting(t *testing.T) {
	existing := sdkhttpclient.NamedMiddlewareFunc("existing", func(_ sdkhttpclient.Options, next http.RoundTripper) http.RoundTripper {
		return next
	})
	ctx := sdkhttpclient.WithContextualMiddleware(context.Background(), existing)

	ctx = (&Datasource{}).contextualMiddlewares(ctx)

	middlewares := sdkhttpclient.ContextualMiddlewareFromContext(ctx)
	require.Len(t, middlewares, 2)
	assert.Equal(t, "existing", middlewares[0].(sdkhttpclient.MiddlewareName).MiddlewareName())
	assert.Equal(t, sdkhttpclient.ResponseLimitMiddlewareName, middlewares[1].(sdkhttpclient.MiddlewareName).MiddlewareName())
}

func TestCheckHealth_InvalidAccount(t *testing.T) {
	tests := []struct {
		name    string
		account string
		message string
	}{
		{
			name:    "full URL",
			account: "https://acme.logicmonitor.com",
			message: `Invalid LogicMonitor account "https://acme.logicmonitor.com". Enter only "acme", not the full domain "acme.logicmonitor.com".`,
		},
		{
			name:    "invalid characters",
			account: "acme_corp",
			message: `Invalid LogicMonitor account "acme_corp". Use only lowercase letters, numbers and hyphens, without a leading or trailing hyphen.`,
		},
		{
			name:    "missing",
			account: "",
			message: `LogicMonitor account is not configured. Enter only the account name, e.g. "acme" for https://acme.logicmonitor.com.`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst, err := NewDatasource(context.Background(), backend.DataSourceInstanceSettings{})
			require.NoError(t, err)

			res, err := inst.(*Datasource).CheckHealth(context.Background(), &backend.CheckHealthRequest{
				PluginContext: backend.PluginContext{
					DataSourceInstanceSettings: &backend.DataSourceInstanceSettings{
						JSONData: []byte(`{"account":"` + tt.account + `"}`),
					},
				},
			})
			require.NoError(t, err)
			assert.Equal(t, backend.HealthStatusError, res.Status)
			assert.Equal(t, tt.message, res.Message)
		})
	}

	t.Run("missing bearer token fails fast before querying", func(t *testing.T) {
		inst, err := NewDatasource(context.Background(), backend.DataSourceInstanceSettings{})
		require.NoError(t, err)

		res, err := inst.(*Datasource).CheckHealth(context.Background(), &backend.CheckHealthRequest{
			PluginContext: backend.PluginContext{
				DataSourceInstanceSettings: &backend.DataSourceInstanceSettings{
					JSONData: []byte(`{"account":"acme","authType":"bearer"}`),
				},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, backend.HealthStatusError, res.Status)
		assert.Contains(t, res.Message, "LogicMonitor bearer token is not configured")
	})
}

type mockRoundTripper struct {
	statusCode int
	body       []byte
	headers    http.Header
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	header := http.Header{"Content-Type": []string{"application/json"}}
	for k, vals := range m.headers {
		header[http.CanonicalHeaderKey(k)] = append([]string(nil), vals...)
	}
	return &http.Response{
		StatusCode: m.statusCode,
		Status:     http.StatusText(m.statusCode),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(string(m.body))),
		Request:    req,
	}, nil
}

func TestCheckHealth_EndToEnd_WithErrorMessage(t *testing.T) {
	plog := backend.NewLoggerWith("logger", "test")
	mockBody := []byte(`{"errorMessage":"OTLP Metrics query feature is not enabled.","errorCode":1400,"errorDetail":null}`)
	provider := sdkhttpclient.NewProvider()

	ds := &Datasource{
		Service: promlib.NewService(provider, plog, func(ctx context.Context, settings backend.DataSourceInstanceSettings, opts *sdkhttpclient.Options, logger log.Logger) error {
			if err := lmadapter.ExtendOptions(ctx, settings, opts, logger); err != nil {
				return err
			}
			opts.Middlewares = append(opts.Middlewares, sdkhttpclient.NamedMiddlewareFunc("mock-backend", func(_ sdkhttpclient.Options, _ http.RoundTripper) http.RoundTripper {
				return &mockRoundTripper{statusCode: http.StatusBadRequest, body: mockBody}
			}))
			return nil
		}),
	}

	res, err := ds.CheckHealth(context.Background(), &backend.CheckHealthRequest{
		PluginContext: backend.PluginContext{
			GrafanaConfig: config.NewGrafanaCfg(nil),
			DataSourceInstanceSettings: &backend.DataSourceInstanceSettings{
				JSONData: []byte(`{"account":"acme","authType":"bearer"}`),
				DecryptedSecureJSONData: map[string]string{
					"token": "test-token",
				},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, backend.HealthStatusError, res.Status)
	assert.Equal(t, `Bad Request - There was an error returned querying the LogicMonitor API: "OTLP Metrics query feature is not enabled".`, res.Message)
}

func TestCheckHealth_EndToEnd_WithoutErrorMessage(t *testing.T) {
	plog := backend.NewLoggerWith("logger", "test")
	mockBody := []byte(`{"errorCode":1400}`)
	provider := sdkhttpclient.NewProvider()

	ds := &Datasource{
		Service: promlib.NewService(provider, plog, func(ctx context.Context, settings backend.DataSourceInstanceSettings, opts *sdkhttpclient.Options, logger log.Logger) error {
			if err := lmadapter.ExtendOptions(ctx, settings, opts, logger); err != nil {
				return err
			}
			opts.Middlewares = append(opts.Middlewares, sdkhttpclient.NamedMiddlewareFunc("mock-backend", func(_ sdkhttpclient.Options, _ http.RoundTripper) http.RoundTripper {
				return &mockRoundTripper{statusCode: http.StatusBadRequest, body: mockBody}
			}))
			return nil
		}),
	}

	res, err := ds.CheckHealth(context.Background(), &backend.CheckHealthRequest{
		PluginContext: backend.PluginContext{
			GrafanaConfig: config.NewGrafanaCfg(nil),
			DataSourceInstanceSettings: &backend.DataSourceInstanceSettings{
				JSONData: []byte(`{"account":"acme","authType":"bearer"}`),
				DecryptedSecureJSONData: map[string]string{
					"token": "test-token",
				},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, backend.HealthStatusError, res.Status)
	assert.Equal(t, `Bad Request - There was an error returned querying the LogicMonitor API.`, res.Message)
}

func TestCheckHealth_EndToEnd_Success(t *testing.T) {
	plog := backend.NewLoggerWith("logger", "test")
	mockBody := []byte(`{"data":[{"metric":{"__name__":"up"},"value":{"timestamp":1600000000,"value":"1"}}],"meta":{"resultType":"vector"}}`)
	provider := sdkhttpclient.NewProvider()

	ds := &Datasource{
		Service: promlib.NewService(provider, plog, func(ctx context.Context, settings backend.DataSourceInstanceSettings, opts *sdkhttpclient.Options, logger log.Logger) error {
			if err := lmadapter.ExtendOptions(ctx, settings, opts, logger); err != nil {
				return err
			}
			opts.Middlewares = append(opts.Middlewares, sdkhttpclient.NamedMiddlewareFunc("mock-backend", func(_ sdkhttpclient.Options, _ http.RoundTripper) http.RoundTripper {
				return &mockRoundTripper{statusCode: http.StatusOK, body: mockBody}
			}))
			return nil
		}),
	}

	res, err := ds.CheckHealth(context.Background(), &backend.CheckHealthRequest{
		PluginContext: backend.PluginContext{
			GrafanaConfig: config.NewGrafanaCfg(nil),
			DataSourceInstanceSettings: &backend.DataSourceInstanceSettings{
				JSONData: []byte(`{"account":"acme","authType":"bearer"}`),
				DecryptedSecureJSONData: map[string]string{
					"token": "test-token",
				},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, backend.HealthStatusOk, res.Status)
	assert.Equal(t, `Successfully queried the LogicMonitor API.`, res.Message)
}

func TestCheckHealth_EndToEnd_ServerVersion(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       []byte
		version    string
		wantStatus backend.HealthStatus
		wantMsg    string
	}{
		{
			name:       "success includes LM version from x-server-version header",
			statusCode: http.StatusOK,
			body:       []byte(`{"data":[{"metric":{"__name__":"up"},"value":{"timestamp":1600000000,"value":"1"}}],"meta":{"resultType":"vector"}}`),
			version:    "247-3",
			wantStatus: backend.HealthStatusOk,
			wantMsg:    `Successfully queried the LogicMonitor API. Detected LogicMonitor version: 247-3.`,
		},
		{
			name:       "error with LM message includes LM version",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"errorMessage":"OTLP Metrics query feature is not enabled.","errorCode":1400,"errorDetail":null}`),
			version:    "247-3",
			wantStatus: backend.HealthStatusError,
			wantMsg:    `Bad Request - There was an error returned querying the LogicMonitor API: "OTLP Metrics query feature is not enabled". Detected LogicMonitor version: 247-3.`,
		},
		{
			name:       "error without LM message includes LM version",
			statusCode: http.StatusUnauthorized,
			body:       []byte(`{"errorCode":1401}`),
			version:    "247-3",
			wantStatus: backend.HealthStatusError,
			wantMsg:    `Unauthorized - There was an error returned querying the LogicMonitor API. Detected LogicMonitor version: 247-3.`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plog := backend.NewLoggerWith("logger", "test")
			provider := sdkhttpclient.NewProvider()
			headers := http.Header{}
			headers.Set("x-server-version", tc.version)

			ds := &Datasource{
				Service: promlib.NewService(provider, plog, func(ctx context.Context, settings backend.DataSourceInstanceSettings, opts *sdkhttpclient.Options, logger log.Logger) error {
					if err := lmadapter.ExtendOptions(ctx, settings, opts, logger); err != nil {
						return err
					}
					opts.Middlewares = append(opts.Middlewares, sdkhttpclient.NamedMiddlewareFunc("mock-backend", func(_ sdkhttpclient.Options, _ http.RoundTripper) http.RoundTripper {
						return &mockRoundTripper{statusCode: tc.statusCode, body: tc.body, headers: headers}
					}))
					return nil
				}),
			}

			res, err := ds.CheckHealth(context.Background(), &backend.CheckHealthRequest{
				PluginContext: backend.PluginContext{
					GrafanaConfig: config.NewGrafanaCfg(nil),
					DataSourceInstanceSettings: &backend.DataSourceInstanceSettings{
						JSONData:                []byte(`{"account":"acme","authType":"bearer"}`),
						DecryptedSecureJSONData: map[string]string{"token": "test-token"},
					},
				},
			})
			require.NoError(t, err)
			require.NotNil(t, res)
			assert.Equal(t, tc.wantStatus, res.Status)
			assert.Equal(t, tc.wantMsg, res.Message)
		})
	}
}

func TestFormatHealthCheckResult(t *testing.T) {
	t.Run("success response replaces Prometheus with LogicMonitor", func(t *testing.T) {
		res := &backend.CheckHealthResult{
			Status:  backend.HealthStatusOk,
			Message: "Successfully queried the Prometheus API.",
		}
		formatted, err := formatHealthCheckResult(res, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, "Successfully queried the LogicMonitor API.", formatted.Message)
		assert.Equal(t, backend.HealthStatusOk, formatted.Status)
	})

	t.Run("error response with tracker message appends quoted message", func(t *testing.T) {
		res := &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "Bad Request - There was an error returned querying the Prometheus API.",
		}
		tracker := &lm.HealthCheckTracker{}
		tracker.SetErrorMessage("OTLP Metrics query feature is not enabled.")
		formatted, err := formatHealthCheckResult(res, nil, tracker)
		require.NoError(t, err)
		assert.Equal(t, `Bad Request - There was an error returned querying the LogicMonitor API: "OTLP Metrics query feature is not enabled".`, formatted.Message)
	})

	t.Run("error response without tracker message", func(t *testing.T) {
		res := &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "Bad Request - There was an error returned querying the Prometheus API.",
		}
		formatted, err := formatHealthCheckResult(res, nil, &lm.HealthCheckTracker{})
		require.NoError(t, err)
		assert.Equal(t, "Bad Request - There was an error returned querying the LogicMonitor API.", formatted.Message)
	})

	t.Run("err not nil with tracker message", func(t *testing.T) {
		tracker := &lm.HealthCheckTracker{}
		tracker.SetErrorMessage("some details")
		formatted, err := formatHealthCheckResult(nil, errors.New("custom error"), tracker)
		require.NoError(t, err)
		assert.Equal(t, `custom error - There was an error returned querying the LogicMonitor API: "some details".`, formatted.Message)
		assert.Equal(t, backend.HealthStatusError, formatted.Status)
	})

	t.Run("nil result and nil err", func(t *testing.T) {
		formatted, err := formatHealthCheckResult(nil, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, "There was an error returned querying the LogicMonitor API.", formatted.Message)
		assert.Equal(t, backend.HealthStatusError, formatted.Status)
	})

	t.Run("appends server version on success and failure", func(t *testing.T) {
		tracker := &lm.HealthCheckTracker{}
		tracker.SetServerVersion("247-3")

		okRes, err := formatHealthCheckResult(&backend.CheckHealthResult{
			Status:  backend.HealthStatusOk,
			Message: "Successfully queried the Prometheus API.",
		}, nil, tracker)
		require.NoError(t, err)
		assert.Equal(t, "Successfully queried the LogicMonitor API. Detected LogicMonitor version: 247-3.", okRes.Message)

		tracker.SetErrorMessage("bad token")
		errRes, err := formatHealthCheckResult(nil, errors.New("Unauthorized"), tracker)
		require.NoError(t, err)
		assert.Equal(t, `Unauthorized - There was an error returned querying the LogicMonitor API: "bad token". Detected LogicMonitor version: 247-3.`, errRes.Message)
	})
}

func TestValidateAdmission(t *testing.T) {
	ds := &Datasource{}

	t.Run("valid ObjectBytes passes", func(t *testing.T) {
		res, err := ds.ValidateAdmission(context.Background(), &backend.AdmissionRequest{
			ObjectBytes: []byte(`{"spec":{"jsonData":{"account":"acme","authType":"bearer"},"secureJsonData":{"token":"secret"}}}`),
		})
		require.NoError(t, err)
		assert.True(t, res.Allowed)
	})

	t.Run("invalid account in ObjectBytes fails", func(t *testing.T) {
		res, err := ds.ValidateAdmission(context.Background(), &backend.AdmissionRequest{
			ObjectBytes: []byte(`{"jsonData":{"account":"https://acme.logicmonitor.com"}}`),
		})
		require.NoError(t, err)
		assert.False(t, res.Allowed)
		require.NotNil(t, res.Result)
		assert.Contains(t, res.Result.Message, `Enter only "acme"`)
	})

	t.Run("missing credentials in ObjectBytes fails", func(t *testing.T) {
		res, err := ds.ValidateAdmission(context.Background(), &backend.AdmissionRequest{
			ObjectBytes: []byte(`{"jsonData":{"account":"acme","authType":"bearer"}}`),
		})
		require.NoError(t, err)
		assert.False(t, res.Allowed)
		require.NotNil(t, res.Result)
		assert.Contains(t, res.Result.Message, "bearer token is not configured")
	})

	t.Run("MutateAdmission allows request", func(t *testing.T) {
		res, err := ds.MutateAdmission(context.Background(), &backend.AdmissionRequest{})
		require.NoError(t, err)
		assert.True(t, res.Allowed)
	})
}
