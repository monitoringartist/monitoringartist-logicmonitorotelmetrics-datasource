package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	sdkhttpclient "github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/config"
	promlib "github.com/grafana/grafana-prometheus-datasource/pkg/promlib"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lmadapter"
)

// NewDatasource creates the datasource with upstream Prometheus processing and
// a narrow LogicMonitor HTTP-translation middleware.
func NewDatasource(_ context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	plog := backend.NewLoggerWith("logger", "tsdb.prometheus")
	plog.Debug("Initializing")
	var shared *lm.SharedState
	if cfg, err := lm.NewConfigFromSettings(settings); err == nil {
		shared = lm.NewSharedState(cfg)
	}
	return &Datasource{
		Service: promlib.NewService(sdkhttpclient.NewProvider(), plog, lmadapter.ExtendOptionsWithSharedState(shared)),
	}, nil
}

type Datasource struct {
	Service *promlib.Service
}

func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	ctx = d.contextualMiddlewares(ctx)
	request := *req
	request.PluginContext = withPrometheusURL(request.PluginContext)
	return d.Service.QueryData(ctx, &request)
}

func (d *Datasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	ctx = d.contextualMiddlewares(ctx)
	request := *req
	request.PluginContext = withPrometheusURL(request.PluginContext)
	return d.Service.CallResource(ctx, &request, sender)
}

func (d *Datasource) CheckHealth(ctx context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if settings := req.PluginContext.DataSourceInstanceSettings; settings != nil {
		cfg, err := lm.NewConfigFromSettings(*settings)
		if err != nil {
			return &backend.CheckHealthResult{
				Status:  backend.HealthStatusError,
				Message: settingsErrorMessage(err),
			}, nil
		}
		if err := cfg.Validate(); err != nil {
			return &backend.CheckHealthResult{
				Status:  backend.HealthStatusError,
				Message: err.Error(),
			}, nil
		}
	}
	ctx = d.contextualMiddlewares(ctx)
	tracker := &lm.HealthCheckTracker{}
	ctx = lm.WithHealthCheckTracker(ctx, tracker)

	request := *req
	request.PluginContext = withPrometheusURL(request.PluginContext)
	res, err := d.Service.CheckHealth(ctx, &request)
	return formatHealthCheckResult(res, err, tracker)
}

func formatHealthCheckResult(res *backend.CheckHealthResult, err error, tracker *lm.HealthCheckTracker) (*backend.CheckHealthResult, error) {
	if err != nil {
		base := fmt.Sprintf("%s - There was an error returned querying the LogicMonitor API", err.Error())
		if tracker != nil && tracker.ErrorMessage() != "" {
			errMsg := strings.Trim(tracker.ErrorMessage(), `".`)
			return &backend.CheckHealthResult{
				Status:  backend.HealthStatusError,
				Message: fmt.Sprintf(`%s: "%s".`, base, errMsg),
			}, nil
		}
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: base + ".",
		}, nil
	}
	if res == nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "There was an error returned querying the LogicMonitor API.",
		}, nil
	}

	res.Message = replacePrometheus(res.Message)

	if res.Status != backend.HealthStatusOk && tracker != nil && tracker.ErrorMessage() != "" {
		errMsg := strings.Trim(tracker.ErrorMessage(), `".`)
		base := strings.TrimSuffix(res.Message, ".")
		if !strings.Contains(base, errMsg) {
			res.Message = fmt.Sprintf(`%s: "%s".`, base, errMsg)
		}
	}

	return res, nil
}

func replacePrometheus(msg string) string {
	msg = strings.ReplaceAll(msg, "Prometheus API", "LogicMonitor API")
	msg = strings.ReplaceAll(msg, "prometheus API", "LogicMonitor API")
	msg = strings.ReplaceAll(msg, "Prometheus", "LogicMonitor")
	msg = strings.ReplaceAll(msg, "prometheus", "LogicMonitor")
	return msg
}

func settingsErrorMessage(err error) string {
	var accountErr *lm.InvalidAccountError
	if !errors.As(err, &accountErr) {
		return fmt.Sprintf("Could not load the data source settings: %s", err.Error())
	}
	if accountErr.Value == "" {
		return `LogicMonitor account is not configured. Enter only the account name, e.g. "acme" for https://acme.logicmonitor.com.`
	}
	if strings.ContainsAny(accountErr.Value, ".:/@") {
		return fmt.Sprintf(`Invalid LogicMonitor account %q. Enter only "acme", not the full domain "acme.logicmonitor.com".`, accountErr.Value)
	}
	return fmt.Sprintf("Invalid LogicMonitor account %q. Use only lowercase letters, numbers and hyphens, without a leading or trailing hyphen.", accountErr.Value)
}

func (d *Datasource) ValidateAdmission(ctx context.Context, req *backend.AdmissionRequest) (*backend.ValidationResponse, error) {
	settings, err := settingsFromAdmissionRequest(req)
	if err != nil {
		return &backend.ValidationResponse{Allowed: false, Result: &backend.StatusResult{Message: fmt.Sprintf("Could not parse admission request: %s", err.Error())}}, nil
	}
	if settings != nil {
		cfg, err := lm.NewConfigFromSettings(*settings)
		if err != nil {
			return &backend.ValidationResponse{Allowed: false, Result: &backend.StatusResult{Message: settingsErrorMessage(err)}}, nil
		}
		if err := cfg.Validate(); err != nil {
			return &backend.ValidationResponse{Allowed: false, Result: &backend.StatusResult{Message: err.Error()}}, nil
		}
	}
	return &backend.ValidationResponse{Allowed: true}, nil
}

func settingsFromAdmissionRequest(req *backend.AdmissionRequest) (*backend.DataSourceInstanceSettings, error) {
	if len(req.ObjectBytes) > 0 {
		var obj struct {
			URL            string            `json:"url"`
			JSONData       json.RawMessage   `json:"jsonData"`
			SecureJSONData map[string]string `json:"secureJsonData"`
			Spec           *struct {
				URL            string            `json:"url"`
				JSONData       json.RawMessage   `json:"jsonData"`
				SecureJSONData map[string]string `json:"secureJsonData"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(req.ObjectBytes, &obj); err != nil {
			return nil, err
		}
		s := backend.DataSourceInstanceSettings{
			URL:                     obj.URL,
			JSONData:                obj.JSONData,
			DecryptedSecureJSONData: obj.SecureJSONData,
		}
		if obj.Spec != nil {
			if s.URL == "" {
				s.URL = obj.Spec.URL
			}
			if len(s.JSONData) == 0 {
				s.JSONData = obj.Spec.JSONData
			}
			if len(s.DecryptedSecureJSONData) == 0 {
				s.DecryptedSecureJSONData = obj.Spec.SecureJSONData
			}
		}
		if len(s.JSONData) > 0 || len(s.DecryptedSecureJSONData) > 0 || s.URL != "" {
			return &s, nil
		}
	}
	return req.PluginContext.DataSourceInstanceSettings, nil
}

func (d *Datasource) MutateAdmission(ctx context.Context, req *backend.AdmissionRequest) (*backend.MutationResponse, error) {
	return &backend.MutationResponse{Allowed: true}, nil
}

// promlib constructs requests from datasource settings and requires a valid
// URL even though LogicMonitor's endpoint is selected by the adapter. Preserve
// a configured URL; only supply a non-routable placeholder when it is absent.
func withPrometheusURL(pluginContext backend.PluginContext) backend.PluginContext {
	settings := pluginContext.DataSourceInstanceSettings
	if settings == nil || settings.URL != "" {
		return pluginContext
	}
	settingsCopy := *settings
	settingsCopy.URL = "http://prometheus.invalid"
	pluginContext.DataSourceInstanceSettings = &settingsCopy
	return pluginContext
}

func (d *Datasource) contextualMiddlewares(ctx context.Context) context.Context {
	cfg := config.GrafanaConfigFromContext(ctx)
	middlewares := []sdkhttpclient.Middleware{
		sdkhttpclient.ResponseLimitMiddleware(cfg.ResponseLimit()),
	}
	return sdkhttpclient.WithContextualMiddleware(ctx, middlewares...)
}
