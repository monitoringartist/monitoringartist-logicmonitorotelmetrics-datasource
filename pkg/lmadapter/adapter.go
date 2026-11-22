// Package lmadapter routes upstream promlib HTTP requests through the
// LogicMonitor translator. Prometheus parsing and response framing stay in
// Grafana's published promlib module.
package lmadapter

import (
	"context"
	"io"
	"net/http"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	sdkhttpclient "github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

// ExtendOptions installs the LogicMonitor bridge as the HTTP transport used by
// promlib. The configured Grafana transport is still used for LM requests, so
// its proxy and TLS configuration are preserved.
func ExtendOptions(
	ctx context.Context,
	settings backend.DataSourceInstanceSettings,
	opts *sdkhttpclient.Options,
	logger log.Logger,
) error {
	return extendOptions(ctx, settings, opts, logger, nil)
}

// ExtendOptionsWithSharedState returns an ExtendOptionsFunc bound to a
// datasource instance's SharedState so state follows Grafana's instance lifecycle.
func ExtendOptionsWithSharedState(shared *lm.SharedState) func(context.Context, backend.DataSourceInstanceSettings, *sdkhttpclient.Options, log.Logger) error {
	return func(ctx context.Context, settings backend.DataSourceInstanceSettings, opts *sdkhttpclient.Options, logger log.Logger) error {
		return extendOptions(ctx, settings, opts, logger, shared)
	}
}

func extendOptions(
	_ context.Context,
	settings backend.DataSourceInstanceSettings,
	opts *sdkhttpclient.Options,
	logger log.Logger,
	shared *lm.SharedState,
) error {
	cfg, err := lm.NewConfigFromSettings(settings)
	if err != nil {
		return err
	}

	if cfg.IsMockBaseURLActive() {
		if opts.TLS == nil {
			opts.TLS = &sdkhttpclient.TLSOptions{}
		}
		opts.TLS.InsecureSkipVerify = true
	}

	opts.Middlewares = append(opts.Middlewares,
		sdkhttpclient.NamedMiddlewareFunc("logicmonitor-otelmetrics-adapter",
			func(_ sdkhttpclient.Options, next http.RoundTripper) http.RoundTripper {
				doer := roundTripperDoer{
					transport: next,
					shared:    shared,
				}
				bridge := lm.NewBridge(
					lm.NewClient(cfg, doer, logger),
					logger,
				)

				return sdkhttpclient.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
					var body []byte
					if req.Body != nil {
						var readErr error
						body, readErr = io.ReadAll(req.Body)
						if readErr != nil {
							return nil, readErr
						}
						_ = req.Body.Close()
					}

					headers := make(map[string][]string, len(req.Header))
					for name, values := range req.Header {
						headers[name] = append([]string(nil), values...)
					}

					return bridge.QueryResource(req.Context(), &backend.CallResourceRequest{
						Path:    req.URL.Path,
						URL:     req.URL.RequestURI(),
						Method:  req.Method,
						Headers: headers,
						Body:    body,
					})
				})
			}),
	)
	return nil
}

type roundTripperDoer struct {
	transport http.RoundTripper
	shared    *lm.SharedState
}

func (d roundTripperDoer) Do(req *http.Request) (*http.Response, error) {
	return d.transport.RoundTrip(req)
}

func (d roundTripperDoer) SharedState() *lm.SharedState {
	return d.shared
}
