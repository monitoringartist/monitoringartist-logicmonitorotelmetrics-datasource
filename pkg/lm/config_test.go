package lm_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/stretchr/testify/require"

	"github.com/monitoringartist/monitoringartist-logicmonitorotelmetrics-datasource/pkg/lm"
)

func TestNewConfigFromSettings(t *testing.T) {
	t.Run("prefers dedicated jsonData and secureJsonData fields", func(t *testing.T) {
		jsonData, err := json.Marshal(map[string]interface{}{
			"account":      "custom",
			"accessId":     "customAccessID",
			"queryTimeout": "45s",
			"debug":        true,
		})
		require.NoError(t, err)

		settings := backend.DataSourceInstanceSettings{
			URL:           "https://fallback.logicmonitor.com",
			BasicAuthUser: "fallbackUser",
			JSONData:      jsonData,
			DecryptedSecureJSONData: map[string]string{
				"accessKey": "customAccessKey",
				"token":     "Bearer lmb_abc",
			},
		}

		cfg, err := lm.NewConfigFromSettings(settings)
		require.NoError(t, err)
		require.Equal(t, "custom", cfg.Account)
		require.Equal(t, "https://custom.logicmonitor.com", cfg.BaseURL())
		require.Equal(t, "customAccessID", cfg.AccessID)
		require.Equal(t, "customAccessKey", cfg.AccessKey)
		require.Equal(t, "lmb_abc", cfg.BearerToken)
		require.Equal(t, 60*time.Second, cfg.RequestTimeout)
		require.NoError(t, cfg.Validate())
	})

	t.Run("validation fails if credentials missing", func(t *testing.T) {
		cfg, err := lm.NewConfigFromSettings(backend.DataSourceInstanceSettings{
			JSONData: []byte(`{"account":"company"}`),
		})
		require.NoError(t, err)
		require.Error(t, cfg.Validate())
	})

	t.Run("account name expands to the LogicMonitor portal", func(t *testing.T) {
		cfg, err := lm.NewConfigFromSettings(backend.DataSourceInstanceSettings{
			URL:                     "https://ignored.logicmonitor.com",
			JSONData:                []byte(`{"account":"kkr","portal":"old.logicmonitor.com","authType":"bearer"}`),
			DecryptedSecureJSONData: map[string]string{"token": "lmb_x"},
		})
		require.NoError(t, err)
		require.Equal(t, "kkr", cfg.Account)
		require.Equal(t, "https://kkr.logicmonitor.com", cfg.BaseURL())
		require.Equal(t, lm.AuthTypeBearer, cfg.EffectiveAuthType())
		require.Equal(t, "lmb_x", cfg.BearerToken)
		require.NoError(t, cfg.Validate())
	})

	t.Run("lmv1 auth type uses access ID and access key even when a token is saved", func(t *testing.T) {
		cfg, err := lm.NewConfigFromSettings(backend.DataSourceInstanceSettings{
			JSONData:                []byte(`{"account":"kkr","authType":"lmv1","accessId":"id"}`),
			DecryptedSecureJSONData: map[string]string{"token": "tok", "accessKey": "key"},
		})
		require.NoError(t, err)
		require.Equal(t, lm.AuthTypeLMv1, cfg.EffectiveAuthType())
		require.NoError(t, cfg.Validate())
	})

	t.Run("legacy settings without authType prefer the token, then access ID and key", func(t *testing.T) {
		cfg, err := lm.NewConfigFromSettings(backend.DataSourceInstanceSettings{
			JSONData:                []byte(`{"portal":"acme.logicmonitor.com","accessId":"id"}`),
			DecryptedSecureJSONData: map[string]string{"token": "tok", "accessKey": "key"},
		})
		require.NoError(t, err)
		require.Equal(t, "acme", cfg.Account)
		require.Equal(t, lm.AuthTypeBearer, cfg.EffectiveAuthType())

		cfg, err = lm.NewConfigFromSettings(backend.DataSourceInstanceSettings{
			JSONData:                []byte(`{"portal":"acme.logicmonitor.com","accessId":"id"}`),
			DecryptedSecureJSONData: map[string]string{"accessKey": "key"},
		})
		require.NoError(t, err)
		require.Equal(t, lm.AuthTypeLMv1, cfg.EffectiveAuthType())
		require.NoError(t, cfg.Validate())
	})

	t.Run("lmTimeout defaults to 60s and ignores out-of-range values", func(t *testing.T) {
		cases := map[string]time.Duration{
			``:                      60 * time.Second,
			`,"queryTimeout":"45s"`: 60 * time.Second,
			`,"lmTimeout":1`:        1 * time.Second,
			`,"lmTimeout":600`:      600 * time.Second,
			`,"lmTimeout":"90"`:     90 * time.Second,
			`,"lmTimeout":0`:        60 * time.Second,
			`,"lmTimeout":-5`:       60 * time.Second,
			`,"lmTimeout":601`:      60 * time.Second,
			`,"lmTimeout":1.5`:      60 * time.Second,
			`,"lmTimeout":"abc"`:    60 * time.Second,
		}
		for extra, want := range cases {
			jsonData := `{"account":"acme"` + extra + `}`
			cfg, err := lm.NewConfigFromSettings(backend.DataSourceInstanceSettings{JSONData: []byte(jsonData)})
			require.NoError(t, err)
			require.Equal(t, want, cfg.RequestTimeout, jsonData)
		}
	})

	t.Run("advanced settings default to the built-in limits", func(t *testing.T) {
		cfg, err := lm.NewConfigFromSettings(backend.DataSourceInstanceSettings{JSONData: []byte(`{"account":"acme"}`)})
		require.NoError(t, err)
		require.Equal(t, 10, cfg.QueryRangeWorkers)
		require.Equal(t, 16, cfg.MaxConcurrentRequests)
		require.Equal(t, 3, cfg.MaxRetryAttempts)
		require.Equal(t, 2*time.Second, cfg.MaxRateLimitWait)
		require.Equal(t, 60*time.Second, cfg.CacheTTL)
		require.Equal(t, 1000, cfg.CacheMaxEntries)
	})

	t.Run("advanced settings are parsed and clamped", func(t *testing.T) {
		cases := []struct {
			jsonData string
			want     lm.Config
		}{
			{
				`{"account":"acme","queryRangeWorkers":4,"maxConcurrentRequests":"8","maxRetryAttempts":5,"maxRateLimitWaitSeconds":10,"cacheTTLSeconds":300,"cacheMaxEntries":5000}`,
				lm.Config{QueryRangeWorkers: 4, MaxConcurrentRequests: 8, MaxRetryAttempts: 5, MaxRateLimitWait: 10 * time.Second, CacheTTL: 300 * time.Second, CacheMaxEntries: 5000},
			},
			{
				`{"account":"acme","queryRangeWorkers":0,"maxConcurrentRequests":-1,"maxRetryAttempts":0,"maxRateLimitWaitSeconds":0,"cacheTTLSeconds":0,"cacheMaxEntries":0}`,
				lm.Config{QueryRangeWorkers: 1, MaxConcurrentRequests: 1, MaxRetryAttempts: 1, MaxRateLimitWait: time.Second, CacheTTL: time.Second, CacheMaxEntries: 1},
			},
			{
				`{"account":"acme","queryRangeWorkers":500,"maxConcurrentRequests":1000,"maxRetryAttempts":99,"maxRateLimitWaitSeconds":600,"cacheTTLSeconds":99999,"cacheMaxEntries":9999999}`,
				lm.Config{QueryRangeWorkers: 50, MaxConcurrentRequests: 100, MaxRetryAttempts: 10, MaxRateLimitWait: 30 * time.Second, CacheTTL: time.Hour, CacheMaxEntries: 100000},
			},
			{
				`{"account":"acme","queryRangeWorkers":2.5,"maxConcurrentRequests":"abc","maxRetryAttempts":true,"maxRateLimitWaitSeconds":null,"cacheTTLSeconds":"","cacheMaxEntries":[1]}`,
				lm.Config{QueryRangeWorkers: 10, MaxConcurrentRequests: 16, MaxRetryAttempts: 3, MaxRateLimitWait: 2 * time.Second, CacheTTL: 60 * time.Second, CacheMaxEntries: 1000},
			},
		}
		for _, tc := range cases {
			cfg, err := lm.NewConfigFromSettings(backend.DataSourceInstanceSettings{JSONData: []byte(tc.jsonData)})
			require.NoError(t, err)
			got := lm.Config{
				QueryRangeWorkers:     cfg.QueryRangeWorkers,
				MaxConcurrentRequests: cfg.MaxConcurrentRequests,
				MaxRetryAttempts:      cfg.MaxRetryAttempts,
				MaxRateLimitWait:      cfg.MaxRateLimitWait,
				CacheTTL:              cfg.CacheTTL,
				CacheMaxEntries:       cfg.CacheMaxEntries,
			}
			require.Equal(t, tc.want, got, tc.jsonData)
		}
	})

	t.Run("validation errors name the missing field for each auth type", func(t *testing.T) {
		cases := []struct {
			cfg  lm.Config
			want string
		}{
			{lm.Config{BearerToken: "tok"}, "invalid LogicMonitor account"},
			{lm.Config{Account: "a", AuthType: lm.AuthTypeBearer, AccessID: "id", AccessKey: "key"}, "bearer token is not configured"},
			{lm.Config{Account: "a", AuthType: lm.AuthTypeLMv1, AccessKey: "key", BearerToken: "tok"}, "access ID is not configured"},
			{lm.Config{Account: "a", AuthType: lm.AuthTypeLMv1, AccessID: "id"}, "access key is not configured"},
			{lm.Config{Account: "a", AccessID: "id"}, "access key is not configured"},
			{lm.Config{Account: "a"}, "credentials are not configured"},
		}
		for _, tc := range cases {
			require.ErrorContains(t, tc.cfg.Validate(), tc.want)
		}
	})
}

func TestNewConfigFromSettingsIgnoresCredentialFallbacks(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		user   string
		secure map[string]string
	}{
		{name: "basicAuthUser", user: "basicUser"},
		{name: "basicAuthPassword", secure: map[string]string{"basicAuthPassword": "basicPass"}},
		{name: "bearerToken", secure: map[string]string{"bearerToken": "lmb_legacy"}},
		{name: "httpHeaderValue1", secure: map[string]string{"httpHeaderValue1": "Bearer lmb_header"}},
		{name: "httpHeaderValue2", secure: map[string]string{"httpHeaderValue2": "Bearer lmb_header"}},
		{name: "LM_ACCESS_ID", env: map[string]string{"LM_ACCESS_ID": "envID"}},
		{name: "LM_ACCESS_KEY", env: map[string]string{"LM_ACCESS_KEY": "envKey"}},
		{name: "LM_BEARER_TOKEN", env: map[string]string{"LM_BEARER_TOKEN": "lmb_env"}},
		{
			name:   "all fallbacks together",
			env:    map[string]string{"LM_ACCESS_ID": "envID", "LM_ACCESS_KEY": "envKey", "LM_BEARER_TOKEN": "lmb_env"},
			user:   "basicUser",
			secure: map[string]string{"basicAuthPassword": "basicPass", "bearerToken": "lmb_legacy", "httpHeaderValue1": "Bearer lmb_header"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := lm.NewConfigFromSettings(backend.DataSourceInstanceSettings{
				BasicAuthUser:           tc.user,
				JSONData:                []byte(`{"account":"acme"}`),
				DecryptedSecureJSONData: tc.secure,
			})
			require.NoError(t, err)
			require.Empty(t, cfg.AccessID)
			require.Empty(t, cfg.AccessKey)
			require.Empty(t, cfg.BearerToken)
			require.Empty(t, cfg.EffectiveAuthType())
			require.ErrorContains(t, cfg.Validate(), "credentials are not configured")
		})
	}
}

func TestNewConfigFromSettingsReadsOnlyDedicatedCredentialFields(t *testing.T) {
	cfg, err := lm.NewConfigFromSettings(backend.DataSourceInstanceSettings{
		BasicAuthUser: "basicUser",
		JSONData:      []byte(`{"account":"acme","authType":"LMv1","accessId":"id"}`),
		DecryptedSecureJSONData: map[string]string{
			"accessKey":         "key",
			"token":             "tok",
			"basicAuthPassword": "basicPass",
			"bearerToken":       "lmb_legacy",
			"httpHeaderValue1":  "lmb_header",
		},
	})
	require.NoError(t, err)
	require.Equal(t, lm.AuthTypeLMv1, cfg.AuthType)
	require.Equal(t, "id", cfg.AccessID)
	require.Equal(t, "key", cfg.AccessKey)
	require.Equal(t, "tok", cfg.BearerToken)
}

func TestNewConfigFromSettingsAccount(t *testing.T) {
	cases := []struct {
		name     string
		settings backend.DataSourceInstanceSettings
		want     string
		wantErr  string
	}{
		{name: "valid name", settings: accountSettings(`{"account":"acme"}`), want: "acme"},
		{name: "valid name with hyphen and digits", settings: accountSettings(`{"account":"acme-01"}`), want: "acme-01"},
		{name: "trims and lowercases", settings: accountSettings(`{"account":"  ACME  "}`), want: "acme"},
		{name: "single character", settings: accountSettings(`{"account":"a"}`), want: "a"},
		{name: "63 characters", settings: accountSettings(`{"account":"` + strings.Repeat("a", 63) + `"}`), want: strings.Repeat("a", 63)},
		{name: "64 characters", settings: accountSettings(`{"account":"` + strings.Repeat("a", 64) + `"}`), wantErr: strings.Repeat("a", 64)},
		{name: "domain", settings: accountSettings(`{"account":"evil.com"}`), wantErr: "evil.com"},
		{name: "ip address", settings: accountSettings(`{"account":"127.0.0.1"}`), wantErr: "127.0.0.1"},
		{name: "logicmonitor domain suffix attack", settings: accountSettings(`{"account":"acme.logicmonitor.com.evil.com"}`), wantErr: "acme.logicmonitor.com.evil.com"},
		{name: "full logicmonitor domain", settings: accountSettings(`{"account":"acme.logicmonitor.com"}`), wantErr: "acme.logicmonitor.com"},
		{name: "scheme", settings: accountSettings(`{"account":"http://x"}`), wantErr: "http://x"},
		{name: "port", settings: accountSettings(`{"account":"acme:8080"}`), wantErr: "acme:8080"},
		{name: "path", settings: accountSettings(`{"account":"acme/path"}`), wantErr: "acme/path"},
		{name: "userinfo", settings: accountSettings(`{"account":"a@b"}`), wantErr: "a@b"},
		{name: "leading hyphen", settings: accountSettings(`{"account":"-acme"}`), wantErr: "-acme"},
		{name: "trailing hyphen", settings: accountSettings(`{"account":"acme-"}`), wantErr: "acme-"},
		{name: "empty", settings: accountSettings(`{"account":""}`), wantErr: `""`},
		{name: "whitespace only", settings: accountSettings(`{"account":"   "}`), wantErr: `""`},
		{name: "nothing configured", settings: backend.DataSourceInstanceSettings{}, wantErr: `""`},
		{name: "legacy portal domain", settings: accountSettings(`{"portal":"acme.logicmonitor.com"}`), want: "acme"},
		{name: "legacy portal https url", settings: accountSettings(`{"portal":"https://acme.logicmonitor.com"}`), want: "acme"},
		{name: "legacy portal https url with slash", settings: accountSettings(`{"portal":"https://Acme.LogicMonitor.com/"}`), want: "acme"},
		{name: "legacy datasource url", settings: backend.DataSourceInstanceSettings{URL: "https://acme.logicmonitor.com/"}, want: "acme"},
		{name: "legacy portal http url not migrated", settings: accountSettings(`{"portal":"http://acme.logicmonitor.com"}`), wantErr: `""`},
		{name: "legacy portal with path not migrated", settings: accountSettings(`{"portal":"https://acme.logicmonitor.com/santaba/rest"}`), wantErr: `""`},
		{name: "legacy portal with port not migrated", settings: accountSettings(`{"portal":"https://acme.logicmonitor.com:8443"}`), wantErr: `""`},
		{name: "legacy portal other domain not migrated", settings: accountSettings(`{"portal":"evil.com"}`), wantErr: `""`},
		{name: "legacy datasource url other host not migrated", settings: backend.DataSourceInstanceSettings{URL: "http://127.0.0.1:9090"}, wantErr: `""`},
		{name: "legacy datasource url used when legacy portal is not migratable", settings: backend.DataSourceInstanceSettings{URL: "acme.logicmonitor.com", JSONData: []byte(`{"portal":"evil.com"}`)}, want: "acme"},
		{name: "account wins over legacy portal", settings: accountSettings(`{"account":"new","portal":"old.logicmonitor.com"}`), want: "new"},
		{name: "invalid account is not replaced by legacy portal", settings: accountSettings(`{"account":"evil.com","portal":"old.logicmonitor.com"}`), wantErr: "evil.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LM_PORTAL", "env.logicmonitor.com")
			cfg, err := lm.NewConfigFromSettings(tc.settings)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), "invalid LogicMonitor account ")
				require.Contains(t, err.Error(), tc.wantErr)
				require.Contains(t, err.Error(), `: enter only the account name (e.g. "acme" for https://acme.logicmonitor.com), not a URL or domain`)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Account)
			require.Equal(t, "https://"+tc.want+".logicmonitor.com", cfg.BaseURL())
		})
	}

	t.Run("GF_PLUGIN_LM_MOCK_BASE_URL overrides acme only", func(t *testing.T) {
		t.Setenv("GF_PLUGIN_LM_MOCK_BASE_URL", "http://localhost:8080/")
		acmeCfg := &lm.Config{Account: "acme"}
		require.Equal(t, "http://localhost:8080", acmeCfg.BaseURL())

		otherCfg := &lm.Config{Account: "kkr"}
		require.Equal(t, "https://kkr.logicmonitor.com", otherCfg.BaseURL())
	})
}

func TestParseAccountErrorMessage(t *testing.T) {
	_, err := lm.ParseAccount("x.com")
	require.EqualError(t, err, `invalid LogicMonitor account "x.com": enter only the account name (e.g. "acme" for https://acme.logicmonitor.com), not a URL or domain`)
}

func accountSettings(jsonData string) backend.DataSourceInstanceSettings {
	return backend.DataSourceInstanceSettings{JSONData: []byte(jsonData)}
}
