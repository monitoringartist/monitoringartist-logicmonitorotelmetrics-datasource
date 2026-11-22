package lm

import (
	"encoding/json"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

const (
	AuthTypeBearer = "bearer"
	AuthTypeLMv1   = "lmv1"

	DefaultLMTimeoutSeconds = 60
	MinLMTimeoutSeconds     = 1
	MaxLMTimeoutSeconds     = 600

	DefaultQueryRangeWorkers = 10
	MinQueryRangeWorkers     = 1
	MaxQueryRangeWorkers     = 50

	DefaultMaxConcurrentRequests = 16
	MinMaxConcurrentRequests     = 1
	MaxMaxConcurrentRequests     = 100

	DefaultMaxRetryAttempts = 3
	MinMaxRetryAttempts     = 1
	MaxMaxRetryAttempts     = 10

	DefaultMaxRateLimitWaitSeconds = 2
	MinMaxRateLimitWaitSeconds     = 1
	MaxMaxRateLimitWaitSeconds     = 30

	DefaultCacheTTLSeconds = 60
	MinCacheTTLSeconds     = 1
	MaxCacheTTLSeconds     = 3600

	DefaultCacheMaxEntries = 1000
	MinCacheMaxEntries     = 1
	MaxCacheMaxEntries     = 100000
)

var (
	accountPattern       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	legacyPortalPattern  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)\.logicmonitor\.com$`)
	legacyPortalURLRegex = regexp.MustCompile(`^https://([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)\.logicmonitor\.com/?$`)
)

// Config holds the configuration for connecting to the LogicMonitor REST API.
type Config struct {
	Account        string
	AuthType       string // "bearer", "lmv1" or "" (inferred from credentials)
	AccessID       string
	AccessKey      string
	BearerToken    string
	RequestTimeout time.Duration

	// Zero values use the defaults.
	QueryRangeWorkers     int
	MaxConcurrentRequests int
	MaxRetryAttempts      int
	MaxRateLimitWait      time.Duration
	CacheTTL              time.Duration
	CacheMaxEntries       int
}

// NewConfigFromSettings creates a Config from Grafana's DataSourceInstanceSettings.
func NewConfigFromSettings(settings backend.DataSourceInstanceSettings) (*Config, error) {
	var jsonData map[string]interface{}
	if len(settings.JSONData) > 0 {
		_ = json.Unmarshal(settings.JSONData, &jsonData)
	}
	secure := settings.DecryptedSecureJSONData

	rawAccount := jsonString(jsonData, "account")
	if rawAccount == "" {
		rawAccount = firstNonEmpty(migrateLegacyPortal(jsonString(jsonData, "portal")), migrateLegacyPortal(settings.URL))
	}
	account, err := ParseAccount(rawAccount)
	if err != nil {
		return nil, err
	}

	authType := strings.ToLower(jsonString(jsonData, "authType"))

	accessID := jsonString(jsonData, "accessId")
	accessKey := strings.TrimSpace(secure["accessKey"])
	bearerToken := strings.TrimSpace(secure["token"])
	if len(bearerToken) > 7 && strings.EqualFold(bearerToken[:7], "bearer ") {
		bearerToken = strings.TrimSpace(bearerToken[7:])
	}

	requestTimeout := lmTimeout(jsonData["lmTimeout"])

	return &Config{
		Account:               account,
		AuthType:              authType,
		AccessID:              accessID,
		AccessKey:             accessKey,
		BearerToken:           bearerToken,
		RequestTimeout:        requestTimeout,
		QueryRangeWorkers:     clampedInt(jsonData["queryRangeWorkers"], DefaultQueryRangeWorkers, MinQueryRangeWorkers, MaxQueryRangeWorkers),
		MaxConcurrentRequests: clampedInt(jsonData["maxConcurrentRequests"], DefaultMaxConcurrentRequests, MinMaxConcurrentRequests, MaxMaxConcurrentRequests),
		MaxRetryAttempts:      clampedInt(jsonData["maxRetryAttempts"], DefaultMaxRetryAttempts, MinMaxRetryAttempts, MaxMaxRetryAttempts),
		MaxRateLimitWait:      time.Duration(clampedInt(jsonData["maxRateLimitWaitSeconds"], DefaultMaxRateLimitWaitSeconds, MinMaxRateLimitWaitSeconds, MaxMaxRateLimitWaitSeconds)) * time.Second,
		CacheTTL:              time.Duration(clampedInt(jsonData["cacheTTLSeconds"], DefaultCacheTTLSeconds, MinCacheTTLSeconds, MaxCacheTTLSeconds)) * time.Second,
		CacheMaxEntries:       clampedInt(jsonData["cacheMaxEntries"], DefaultCacheMaxEntries, MinCacheMaxEntries, MaxCacheMaxEntries),
	}, nil
}

// clampedInt reads a whole number from a jsonData value. Missing or invalid
// values return def; out-of-range values are clamped to [lo, hi].
func clampedInt(raw interface{}, def, lo, hi int) int {
	var f float64
	switch v := raw.(type) {
	case float64:
		f = v
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return def
		}
		f = parsed
	default:
		return def
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return def
	}
	return int(max(float64(lo), min(f, float64(hi))))
}

func (c *Config) queryRangeWorkers() int {
	return positiveOr(c.QueryRangeWorkers, DefaultQueryRangeWorkers)
}

func (c *Config) maxConcurrentRequests() int {
	return positiveOr(c.MaxConcurrentRequests, DefaultMaxConcurrentRequests)
}

func (c *Config) maxRetryAttempts() int {
	return positiveOr(c.MaxRetryAttempts, DefaultMaxRetryAttempts)
}

func (c *Config) maxRateLimitWait() time.Duration {
	return positiveOr(c.MaxRateLimitWait, DefaultMaxRateLimitWaitSeconds*time.Second)
}

func (c *Config) cacheTTL() time.Duration {
	return positiveOr(c.CacheTTL, DefaultCacheTTLSeconds*time.Second)
}

func (c *Config) cacheMaxEntries() int {
	return positiveOr(c.CacheMaxEntries, DefaultCacheMaxEntries)
}

func positiveOr[T int | time.Duration](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}

// ParseAccount normalizes and validates a LogicMonitor account name.
func ParseAccount(raw string) (string, error) {
	account := strings.ToLower(strings.TrimSpace(raw))
	if !accountPattern.MatchString(account) {
		return "", &InvalidAccountError{Value: strings.TrimSpace(raw)}
	}
	return account, nil
}

func migrateLegacyPortal(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if m := legacyPortalPattern.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	if m := legacyPortalURLRegex.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return ""
}

// IsMockBaseURLActive reports whether requests for this config are routed
// to a local mock server via GF_PLUGIN_LM_MOCK_BASE_URL.
func (c *Config) IsMockBaseURLActive() bool {
	return c.Account == "acme" && strings.TrimSpace(os.Getenv("GF_PLUGIN_LM_MOCK_BASE_URL")) != ""
}

// BaseURL returns the LogicMonitor portal URL for the configured account.
// When GF_PLUGIN_LM_MOCK_BASE_URL is set (configured via [plugin.<id>]
// lm_mock_base_url in grafana.ini) and the account is "acme", requests are
// routed to that local mock URL instead of https://acme.logicmonitor.com.
func (c *Config) BaseURL() string {
	if c.IsMockBaseURLActive() {
		return strings.TrimRight(strings.TrimSpace(os.Getenv("GF_PLUGIN_LM_MOCK_BASE_URL")), "/")
	}
	return "https://" + c.Account + ".logicmonitor.com"
}

// EffectiveAuthType returns the configured auth type, or infers it from the
// available credentials (bearer token first, then access ID / access key).
func (c *Config) EffectiveAuthType() string {
	switch c.AuthType {
	case AuthTypeBearer, AuthTypeLMv1:
		return c.AuthType
	}
	if c.BearerToken != "" {
		return AuthTypeBearer
	}
	if c.AccessID != "" || c.AccessKey != "" {
		return AuthTypeLMv1
	}
	return ""
}

// Validate checks if the configuration has sufficient information to reach LogicMonitor.
func (c *Config) Validate() error {
	if _, err := ParseAccount(c.Account); err != nil {
		return err
	}
	switch c.EffectiveAuthType() {
	case AuthTypeBearer:
		if c.BearerToken == "" {
			return missingCredentials("LogicMonitor bearer token is not configured: set the Token for Bearer token authentication")
		}
	case AuthTypeLMv1:
		if c.AccessID == "" {
			return missingCredentials("LogicMonitor access ID is not configured: set the Access ID for LMv1 authentication")
		}
		if c.AccessKey == "" {
			return missingCredentials("LogicMonitor access key is not configured: set the Access Key for LMv1 authentication")
		}
	default:
		return missingCredentials("LogicMonitor credentials are not configured: set a bearer token or an access ID and access key")
	}
	return nil
}

func lmTimeout(raw interface{}) time.Duration {
	seconds := float64(DefaultLMTimeoutSeconds)
	switch v := raw.(type) {
	case float64:
		seconds = v
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			seconds = f
		}
	}
	if seconds != math.Trunc(seconds) || seconds < MinLMTimeoutSeconds || seconds > MaxLMTimeoutSeconds {
		seconds = DefaultLMTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

func jsonString(jsonData map[string]interface{}, key string) string {
	s, _ := jsonData[key].(string)
	return strings.TrimSpace(s)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
