// Command lm-mock is a lightweight mock of the LogicMonitor OTel Metrics API
// for local Grafana plugin development and visual testing without a real LM portal.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// flexInt64 accepts both JSON integers/floats and quoted numeric strings,
// matching both the OpenAPI spec (int64) and the plugin client (string).
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		var unquoted string
		if err := json.Unmarshal(b, &unquoted); err != nil {
			return err
		}
		s = strings.TrimSpace(unquoted)
		if s == "" {
			return nil
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexInt64(v)
	return nil
}

type queryRequest struct {
	Query    string    `json:"query"`
	ScopeIDs []int64   `json:"scopeIds,omitempty"`
	Time     flexInt64 `json:"time,omitempty"`
}

type queryRangeRequest struct {
	Query    string    `json:"query"`
	ScopeIDs []int64   `json:"scopeIds,omitempty"`
	Start    flexInt64 `json:"start"`
	End      flexInt64 `json:"end"`
	Step     string    `json:"step,omitempty"`
}

type labelMatcher struct {
	Op     string   `json:"op"`
	Values []string `json:"values"`
}

type labelsRequest struct {
	Metric    string                  `json:"metric,omitempty"`
	Start     int64                   `json:"start,omitempty"`
	End       int64                   `json:"end,omitempty"`
	Limit     int                     `json:"limit,omitempty"`
	LabelName string                  `json:"labelName,omitempty"`
	Filters   map[string]labelMatcher `json:"filters,omitempty"`
}

type labelValuesRequest struct {
	Key     string                  `json:"key"`
	Metric  string                  `json:"metric,omitempty"`
	Start   int64                   `json:"start,omitempty"`
	End     int64                   `json:"end,omitempty"`
	Limit   int                     `json:"limit,omitempty"`
	Filters map[string]labelMatcher `json:"filters,omitempty"`
}

type metricNamesRequest struct {
	Contains string              `json:"contains,omitempty"`
	Start    int64               `json:"start,omitempty"`
	End      int64               `json:"end,omitempty"`
	Limit    int                 `json:"limit,omitempty"`
	Filters  map[string][]string `json:"filters,omitempty"`
}

type apiErrorResponse struct {
	ErrorMessage string  `json:"errorMessage"`
	ErrorCode    int     `json:"errorCode"`
	ErrorDetail  *string `json:"errorDetail"`
}

type lmValue struct {
	Timestamp int64  `json:"timestamp"`
	Value     string `json:"value"`
}

type lmSeriesResult struct {
	Metric map[string]string `json:"metric"`
	Values []lmValue         `json:"values"`
}

type seriesDef struct {
	Labels map[string]string
	Eval   func(tsSec int64) float64
}

// multiScaleWave sums short (~minutes), diurnal (~1d), and weekly (~7-10d)
// harmonics using prime periods so Grafana's step alignment never aliases the
// signal to a flat line at either 1-hour or 30-day zoom levels.
func multiScaleWave(ts int64, base, ampShort, periodShort, ampDay, periodDay, ampWeek, periodWeek, phase float64) float64 {
	t := float64(ts)
	return base +
		ampShort*math.Sin(2*math.Pi*t/periodShort+phase) +
		ampDay*math.Sin(2*math.Pi*t/periodDay+phase*0.7) +
		ampWeek*math.Sin(2*math.Pi*t/periodWeek+phase*1.3)
}

func defaultSeries() []seriesDef {
	return []seriesDef{
		// Special sinus metric: 2 timeseries with different service_instance_id, frequency, and phase.
		{
			Labels: map[string]string{
				"__name__":               "mocked_sinus",
				"service_name":           "signal-generator",
				"service_instance_id":    "sinus-instance-1",
				"deployment_environment": "local",
				"region":                 "us-east-1",
			},
			Eval: func(ts int64) float64 {
				// ~7m + ~24h + ~7d prime-period harmonics, phase 0
				return multiScaleWave(ts, 50, 12, 421, 16, 86413, 18, 604817, 0)
			},
		},
		{
			Labels: map[string]string{
				"__name__":               "mocked_sinus",
				"service_name":           "signal-generator",
				"service_instance_id":    "sinus-instance-2",
				"deployment_environment": "local",
				"region":                 "us-west-2",
			},
			Eval: func(ts int64) float64 {
				// ~11m + ~20h + ~10d prime-period harmonics, phase pi/3
				return multiScaleWave(ts, 50, 10, 659, 14, 72019, 20, 864013, math.Pi/3.0)
			},
		},
		// Realistic OpenTelemetry host & service metrics.
		{
			Labels: map[string]string{
				"__name__":               "mocked_system_cpu_time",
				"service_name":           "orders",
				"service_instance_id":    "orders-01",
				"host_name":              "ip-10-0-1-11",
				"state":                  "user",
				"deployment_environment": "prod",
				"region":                 "us-east-1",
			},
			Eval: func(ts int64) float64 {
				return 12000 + float64(ts%86400)*0.45 + 120*math.Sin(float64(ts)/600.0)
			},
		},
		{
			Labels: map[string]string{
				"__name__":               "mocked_system_cpu_time",
				"service_name":           "orders",
				"service_instance_id":    "orders-01",
				"host_name":              "ip-10-0-1-11",
				"state":                  "system",
				"deployment_environment": "prod",
				"region":                 "us-east-1",
			},
			Eval: func(ts int64) float64 {
				return 4000 + float64(ts%86400)*0.15 + 40*math.Cos(float64(ts)/600.0)
			},
		},
		{
			Labels: map[string]string{
				"__name__":               "mocked_system_cpu_time",
				"service_name":           "checkout",
				"service_instance_id":    "checkout-01",
				"host_name":              "ip-10-0-2-21",
				"state":                  "user",
				"deployment_environment": "prod",
				"region":                 "us-west-2",
			},
			Eval: func(ts int64) float64 {
				return 9500 + float64(ts%86400)*0.32 + 90*math.Sin(float64(ts)/420.0+1.0)
			},
		},
		{
			Labels: map[string]string{
				"__name__":               "mocked_system_memory_usage",
				"service_name":           "orders",
				"service_instance_id":    "orders-01",
				"host_name":              "ip-10-0-1-11",
				"state":                  "used",
				"deployment_environment": "prod",
				"region":                 "us-east-1",
			},
			Eval: func(ts int64) float64 {
				return multiScaleWave(ts, 2.4e9, 0.8e8, 421, 1.5e8, 86413, 2.2e8, 604817, 0)
			},
		},
		{
			Labels: map[string]string{
				"__name__":               "mocked_system_memory_usage",
				"service_name":           "payments",
				"service_instance_id":    "payments-01",
				"host_name":              "ip-10-0-3-31",
				"state":                  "used",
				"deployment_environment": "staging",
				"region":                 "eu-west-1",
			},
			Eval: func(ts int64) float64 {
				return multiScaleWave(ts, 1.6e9, 0.5e8, 659, 1.0e8, 72019, 1.4e8, 864013, 1.1)
			},
		},
		{
			Labels: map[string]string{
				"__name__":               "mocked_http_server_request_duration_seconds_count",
				"service_name":           "orders",
				"service_instance_id":    "orders-01",
				"http_method":            "GET",
				"http_status_code":       "200",
				"deployment_environment": "prod",
				"region":                 "us-east-1",
			},
			Eval: func(ts int64) float64 {
				return 50000 + float64(ts%86400)*12.5 + 300*math.Sin(float64(ts)/300.0)
			},
		},
		{
			Labels: map[string]string{
				"__name__":               "mocked_http_server_request_duration_seconds_count",
				"service_name":           "orders",
				"service_instance_id":    "orders-01",
				"http_method":            "POST",
				"http_status_code":       "500",
				"deployment_environment": "prod",
				"region":                 "us-east-1",
			},
			Eval: func(ts int64) float64 {
				return 250 + float64(ts%86400)*0.08 + 15*math.Sin(float64(ts)/240.0+0.7)
			},
		},
		{
			Labels: map[string]string{
				"__name__":               "mocked_http_server_active_requests",
				"service_name":           "checkout",
				"service_instance_id":    "checkout-01",
				"deployment_environment": "prod",
				"region":                 "us-west-2",
			},
			Eval: func(ts int64) float64 {
				return multiScaleWave(ts, 24, 5, 421, 6, 86413, 6, 604817, 0.2)
			},
		},
		{
			Labels: map[string]string{
				"__name__":               "mocked_http_server_active_requests",
				"service_name":           "payments",
				"service_instance_id":    "payments-01",
				"deployment_environment": "staging",
				"region":                 "eu-west-1",
			},
			Eval: func(ts int64) float64 {
				return multiScaleWave(ts, 14, 3, 659, 4, 72019, 4, 864013, 1.4)
			},
		},
	}
}

type mockServer struct {
	series []seriesDef
	now    func() time.Time
}

func newMockServer() *mockServer {
	return &mockServer{
		series: defaultSeries(),
		now:    time.Now,
	}
}

func (m *mockServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/santaba/rest/metrics/metric-names", m.withAuth(m.handleMetricNames))
	mux.HandleFunc("/santaba/rest/metrics/labels", m.withAuth(m.handleLabels))
	mux.HandleFunc("/santaba/rest/metrics/label-values", m.withAuth(m.handleLabelValues))
	mux.HandleFunc("/santaba/rest/metrics/query", m.withAuth(m.handleQuery))
	mux.HandleFunc("/santaba/rest/metrics/query-range", m.withAuth(m.handleQueryRange))
	return mux
}

func (m *mockServer) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, 1405, "method not allowed")
			return
		}
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if auth == "" || (!strings.HasPrefix(auth, "Bearer ") && !strings.HasPrefix(auth, "LMv1 ")) {
			writeAPIError(w, http.StatusUnauthorized, 1401, "Unauthorized: missing or invalid Authorization header")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAPIError(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, apiErrorResponse{
		ErrorMessage: msg,
		ErrorCode:    code,
	})
}

func (m *mockServer) handleMetricNames(w http.ResponseWriter, r *http.Request) {
	var req metricNamesRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, 1400, err.Error())
		return
	}

	contains := strings.ToLower(strings.TrimSpace(req.Contains))
	seen := make(map[string]struct{})
	for _, s := range m.series {
		if !matchStringFilters(s.Labels, req.Filters) {
			continue
		}
		name := s.Labels["__name__"]
		if contains != "" && !strings.Contains(strings.ToLower(name), contains) {
			continue
		}
		seen[name] = struct{}{}
	}

	names := sortedKeys(seen)
	names = applyLimit(names, req.Limit)
	writeJSON(w, http.StatusOK, map[string]any{"data": names})
}

func (m *mockServer) handleLabels(w http.ResponseWriter, r *http.Request) {
	var req labelsRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, 1400, err.Error())
		return
	}

	sub := strings.ToLower(strings.TrimSpace(req.LabelName))
	seen := make(map[string]struct{})
	for _, s := range m.series {
		if req.Metric != "" && s.Labels["__name__"] != req.Metric {
			continue
		}
		if !matchMatcherFilters(s.Labels, req.Filters) {
			continue
		}
		for k := range s.Labels {
			if sub != "" && !strings.Contains(strings.ToLower(k), sub) {
				continue
			}
			seen[k] = struct{}{}
		}
	}

	out := sortedKeys(seen)
	out = applyLimit(out, req.Limit)
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (m *mockServer) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	var req labelValuesRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, 1400, err.Error())
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		writeAPIError(w, http.StatusBadRequest, 1400, "key is required")
		return
	}

	seen := make(map[string]struct{})
	for _, s := range m.series {
		if req.Metric != "" && s.Labels["__name__"] != req.Metric {
			continue
		}
		if !matchMatcherFilters(s.Labels, req.Filters) {
			continue
		}
		if v, ok := s.Labels[req.Key]; ok {
			seen[v] = struct{}{}
		}
	}

	out := sortedKeys(seen)
	out = applyLimit(out, req.Limit)
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func parsePromQLExpr(query string) (parser.Expr, error) {
	return parser.NewParser(parser.Options{}).ParseExpr(query)
}

func (m *mockServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, 1400, "invalid JSON request body")
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeAPIError(w, http.StatusBadRequest, 1400, "query is required")
		return
	}

	ts := int64(req.Time)
	if ts <= 0 {
		ts = m.now().Unix()
	}

	expr, err := parsePromQLExpr(req.Query)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, 1400, fmt.Sprintf("invalid PromQL query: %v", err))
		return
	}

	if scalarVal, ok := evalScalar(expr); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"resultType": "scalar",
				"result":     []any{ts, formatFloat(scalarVal)},
			},
		})
		return
	}

	matched := m.matchPromQLExpr(expr)
	results := make([]lmSeriesResult, 0, len(matched))
	for _, s := range matched {
		results = append(results, lmSeriesResult{
			Metric: s.Labels,
			Values: []lmValue{{Timestamp: ts, Value: formatFloat(s.Eval(ts))}},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": results})
}

func (m *mockServer) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	var req queryRangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, 1400, "invalid JSON request body")
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeAPIError(w, http.StatusBadRequest, 1400, "query is required")
		return
	}
	start := int64(req.Start)
	end := int64(req.End)
	if end < start {
		writeAPIError(w, http.StatusBadRequest, 1400, "end must not be before start")
		return
	}

	expr, err := parsePromQLExpr(req.Query)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, 1400, fmt.Sprintf("invalid PromQL query: %v", err))
		return
	}

	step := parseStepSeconds(req.Step, start, end)
	matched := m.matchPromQLExpr(expr)

	results := make([]lmSeriesResult, 0, len(matched))
	for _, s := range matched {
		values := make([]lmValue, 0, (end-start)/step+1)
		for ts := start; ts <= end; ts += step {
			values = append(values, lmValue{
				Timestamp: ts,
				Value:     formatFloat(s.Eval(ts)),
			})
		}
		results = append(results, lmSeriesResult{
			Metric: s.Labels,
			Values: values,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": results})
}

func (m *mockServer) matchPromQLExpr(expr parser.Expr) []seriesDef {
	var matchers []*labels.Matcher
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		if vs, ok := node.(*parser.VectorSelector); ok {
			matchers = append(matchers, vs.LabelMatchers...)
		}
		return nil
	})
	if len(matchers) == 0 {
		return nil
	}

	var matched []seriesDef
	for _, s := range m.series {
		lbls := labels.FromMap(s.Labels)
		ok := true
		for _, m := range matchers {
			if !m.Matches(lbls.Get(m.Name)) {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, s)
		}
	}
	return matched
}

func evalScalar(expr parser.Expr) (float64, bool) {
	switch e := expr.(type) {
	case *parser.NumberLiteral:
		return e.Val, true
	case *parser.ParenExpr:
		return evalScalar(e.Expr)
	case *parser.BinaryExpr:
		l, okL := evalScalar(e.LHS)
		r, okR := evalScalar(e.RHS)
		if !okL || !okR {
			return 0, false
		}
		switch e.Op {
		case parser.ADD:
			return l + r, true
		case parser.SUB:
			return l - r, true
		case parser.MUL:
			return l * r, true
		case parser.DIV:
			if r == 0 {
				return math.NaN(), true
			}
			return l / r, true
		}
	}
	return 0, false
}

func parseStepSeconds(raw string, start, end int64) int64 {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if secs, err := strconv.ParseInt(raw, 10, 64); err == nil && secs > 0 {
			return secs
		}
		if d, err := time.ParseDuration(raw); err == nil && d >= time.Second {
			return int64(d / time.Second)
		}
	}
	span := max(end-start, 1)
	return max(span/200, 15)
}

func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil && !errors.Is(err, os.ErrClosed) && err.Error() != "EOF" {
		return fmt.Errorf("invalid JSON request body")
	}
	return nil
}

func matchStringFilters(lbls map[string]string, filters map[string][]string) bool {
	for k, vals := range filters {
		if len(vals) == 0 {
			continue
		}
		actual, ok := lbls[k]
		if !ok {
			return false
		}
		found := false
		for _, v := range vals {
			if actual == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func matchMatcherFilters(lbls map[string]string, filters map[string]labelMatcher) bool {
	for k, matcher := range filters {
		actual := lbls[k]
		if !matchValues(actual, matcher.Op, matcher.Values) {
			return false
		}
	}
	return true
}

func matchValues(actual, op string, values []string) bool {
	if len(values) == 0 {
		return true
	}
	switch op {
	case "", "=":
		for _, v := range values {
			if actual == v {
				return true
			}
		}
		return false
	case "!=":
		for _, v := range values {
			if actual == v {
				return false
			}
		}
		return true
	case "=~":
		for _, v := range values {
			if re, err := regexp.Compile("^(?:" + v + ")$"); err == nil && re.MatchString(actual) {
				return true
			}
		}
		return false
	case "!~":
		for _, v := range values {
			if re, err := regexp.Compile("^(?:" + v + ")$"); err == nil && re.MatchString(actual) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func applyLimit(items []string, limit int) []string {
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 4, 64)
}

func generateSelfSignedTLS(certOutPath string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "*.logicmonitor.com"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"*.logicmonitor.com", "acme.logicmonitor.com", "localhost", "lm-mock"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	if certOutPath != "" {
		if err := os.MkdirAll(filepath.Dir(certOutPath), 0o755); err != nil {
			return tls.Certificate{}, err
		}
		if err := os.WriteFile(certOutPath, certPEM, 0o644); err != nil { //nolint:gosec
			return tls.Certificate{}, err
		}
	}

	return tls.X509KeyPair(certPEM, keyPEM)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	httpAddr := getenv("HTTP_ADDR", ":8080")
	tlsAddr := os.Getenv("TLS_ADDR")
	certOut := os.Getenv("TLS_CERT_OUT")

	srv := newMockServer()
	handler := srv.routes()

	if tlsAddr != "" {
		cert, err := generateSelfSignedTLS(certOut)
		if err != nil {
			log.Fatalf("failed to generate TLS certificate: %v", err)
		}
		tlsSrv := &http.Server{
			Addr:              tlsAddr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			},
		}
		go func() {
			log.Printf("lm-mock HTTPS listening on %s", tlsAddr)
			if err := tlsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("HTTPS server failed: %v", err)
			}
		}()
	}

	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("lm-mock HTTP listening on %s", httpAddr)
	log.Fatal(httpSrv.ListenAndServe())
}
