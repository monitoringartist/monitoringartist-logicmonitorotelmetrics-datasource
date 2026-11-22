package schema_test

import (
	_ "embed"
	"testing"

	"github.com/grafana/dsconfig/schema"
	"github.com/grafana/grafana-plugin-sdk-go/experimental/pluginschema"
	"k8s.io/kube-openapi/pkg/spec3"
)

//go:embed dsconfig.json
var configSchemaJSON []byte

type settingsJSONModel struct {
	Account                 string  `json:"account"`
	AuthType                string  `json:"authType"`
	AccessID                string  `json:"accessId"`
	EnableSecureSocksProxy  bool    `json:"enableSecureSocksProxy"`
	LMTimeout               float64 `json:"lmTimeout"`
	QueryRangeWorkers       float64 `json:"queryRangeWorkers"`
	MaxConcurrentRequests   float64 `json:"maxConcurrentRequests"`
	MaxRetryAttempts        float64 `json:"maxRetryAttempts"`
	MaxRateLimitWaitSeconds float64 `json:"maxRateLimitWaitSeconds"`
	CacheTTLSeconds         float64 `json:"cacheTTLSeconds"`
	CacheMaxEntries         float64 `json:"cacheMaxEntries"`
}

//go:generate go test -run TestPlugin -generateArtifacts
func TestPlugin(t *testing.T) {
	schema.RunPluginTests(t, schema.PluginUnderTest{
		ID:                "monitoringartist-logicmonitorotelmetrics-datasource",
		ConfigSchemaJSON:  configSchemaJSON,
		SettingsJSONModel: settingsJSONModel{},
		SecureKeys:        []string{"token", "accessKey"},
		SettingsExamples: &pluginschema.SettingsExamples{
			Examples: map[string]*spec3.Example{
				"": {
					ExampleProps: spec3.ExampleProps{
						Summary:     "Bearer token authentication",
						Description: "Connect to LogicMonitor using a bearer API token.",
						Value: map[string]any{
							"jsonData": map[string]any{
								"account":  "acme",
								"authType": "bearer",
							},
							"secureJsonData": map[string]any{
								"token": "REPLACE_WITH_BEARER_TOKEN",
							},
						},
					},
				},
				"lmv1": {
					ExampleProps: spec3.ExampleProps{
						Summary:     "LMv1 authentication",
						Description: "Connect to LogicMonitor using an LMv1 access ID and access key.",
						Value: map[string]any{
							"jsonData": map[string]any{
								"account":  "acme",
								"authType": "lmv1",
								"accessId": "REPLACE_WITH_ACCESS_ID",
							},
							"secureJsonData": map[string]any{
								"accessKey": "REPLACE_WITH_ACCESS_KEY",
							},
						},
					},
				},
			},
		},
	})
}
