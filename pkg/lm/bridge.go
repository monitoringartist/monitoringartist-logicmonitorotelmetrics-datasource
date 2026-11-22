package lm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp/syntax"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// Bridge translates Prometheus queries and resource requests to LogicMonitor API calls
// and formats the responses back to Prometheus responses.
type Bridge struct {
	client *Client
	log    log.Logger
}

// NewBridge creates a new Prometheus-LogicMonitor bridge.
func NewBridge(client *Client, plog log.Logger) *Bridge {
	return &Bridge{
		client: client,
		log:    plog,
	}
}

// QueryInstant executes a PromQL instant query against LogicMonitor.
func (b *Bridge) QueryInstant(ctx context.Context, query string, timeStr string) (*http.Response, error) {
	var timestamp int64
	if timeStr != "" {
		seconds, err := strconv.ParseFloat(timeStr, 64)
		if err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
			timestamp = int64(seconds)
		}
	}

	lmReq := &QueryRequest{
		Query: query,
		Time:  timestamp,
	}

	rawResp, err := b.client.Query(ctx, lmReq)
	if err != nil {
		return b.upstreamErrorResponse("failed to execute instant query against LM", err)
	}

	translated, err := TranslateLMResponse(rawResp, "vector")
	if err != nil {
		return makeErrorResponse(http.StatusInternalServerError, "internal", err.Error())
	}

	return makeHTTPResponse(http.StatusOK, translated), nil
}

// QueryRange executes a PromQL range query against LogicMonitor.
func (b *Bridge) QueryRange(ctx context.Context, query, start, end, step string) (*http.Response, error) {
	lmReq := &QueryRequest{
		Query: query,
		Start: start,
		End:   end,
		Step:  step,
	}

	rawResp, err := b.client.QueryRange(ctx, lmReq)
	if err != nil {
		return b.upstreamErrorResponse("failed to execute range query against LM", err)
	}

	translated, err := TranslateLMResponse(rawResp, "matrix")
	if err != nil {
		return makeErrorResponse(http.StatusInternalServerError, "internal", err.Error())
	}

	return makeHTTPResponse(http.StatusOK, translated), nil
}

// QueryResource routes a Grafana CallResourceRequest to the appropriate LogicMonitor handler.
func (b *Bridge) QueryResource(ctx context.Context, req *backend.CallResourceRequest) (*http.Response, error) {
	cleanPath := strings.TrimPrefix(req.Path, "/")

	switch {
	case cleanPath == "api/v1/query":
		query, timeStr := parseInstantQueryRequest(req)
		return b.QueryInstant(ctx, query, timeStr)

	case cleanPath == "api/v1/query_range":
		query, start, end, step := parseRangeQueryRequest(req)
		return b.QueryRange(ctx, query, start, end, step)

	case cleanPath == "api/v1/labels":
		start, end, _, matches, explicitMetric := parseRequestParams(req)
		rawResp, err := b.fetchLabels(ctx, start, end, matches, explicitMetric)
		if err != nil {
			return b.upstreamErrorResponse("failed to fetch labels from LM", err)
		}

		translated, err := TranslateMetadataResponse(rawResp)
		if err != nil {
			return makeErrorResponse(http.StatusInternalServerError, "internal", err.Error())
		}
		return makeHTTPResponse(http.StatusOK, translated), nil

	case strings.HasPrefix(cleanPath, "api/v1/label/"):
		subPath := strings.TrimPrefix(cleanPath, "api/v1/label/")
		parts := strings.Split(subPath, "/")
		if len(parts) >= 2 && parts[1] == "values" {
			labelName := parts[0]
			start, end, limit, matches, explicitMetric := parseRequestParams(req)

			if labelName == "__name__" {
				rawResp, err := b.fetchMetricNames(ctx, start, end, limit, matches, explicitMetric)
				if err != nil {
					return b.upstreamErrorResponse("failed to fetch metric names from LM for __name__", err)
				}

				translated, err := TranslateMetadataResponse(rawResp)
				if err != nil {
					return makeErrorResponse(http.StatusInternalServerError, "internal", err.Error())
				}
				translated, err = filterMetricNames(translated, matches)
				if err != nil {
					return makeErrorResponse(http.StatusInternalServerError, "internal", err.Error())
				}
				return makeHTTPResponse(http.StatusOK, translated), nil
			}

			rawResp, err := b.fetchLabelValues(ctx, labelName, start, end, limit, matches, explicitMetric)
			if err != nil {
				return b.upstreamErrorResponse("failed to fetch label values from LM", err, "label", labelName)
			}

			translated, err := TranslateMetadataResponse(rawResp)
			if err != nil {
				return makeErrorResponse(http.StatusInternalServerError, "internal", err.Error())
			}
			return makeHTTPResponse(http.StatusOK, translated), nil
		}
		return makeErrorResponse(http.StatusNotFound, "bad_data", "not found")

	case cleanPath == "api/v1/series":
		_, end, limit, matches, _ := parseRequestParams(req)
		return b.querySeries(ctx, matches, end, limit)

	case cleanPath == "api/v1/metadata":
		start, end, limit, matches, explicitMetric := parseRequestParams(req)
		rawResp, err := b.fetchMetricNames(ctx, start, end, limit, matches, explicitMetric)
		if err != nil {
			return b.upstreamErrorResponse("failed to fetch metric names for metadata from LM", err)
		}

		translated, err := TranslateMetricMetadataResponse(rawResp)
		if err != nil {
			return makeErrorResponse(http.StatusInternalServerError, "internal", err.Error())
		}
		return makeHTTPResponse(http.StatusOK, translated), nil

	case cleanPath == "api/v1/query_exemplars":
		return makeErrorResponse(http.StatusNotFound, "unavailable", "the Prometheus exemplars API is not supported by LogicMonitor")

	case cleanPath == "api/v1/status/buildinfo":
		buildInfo := PromMetaResponse{
			Status: "success",
			Data: map[string]interface{}{
				"version":   "lm-otel",
				"revision":  "logicmonitor-otel",
				"branch":    "",
				"buildUser": "LogicMonitor",
				"buildDate": "",
				"goVersion": "",
			},
		}
		body, _ := json.Marshal(buildInfo)
		return makeHTTPResponse(http.StatusOK, body), nil

	case strings.HasPrefix(cleanPath, "api/v1/search/"):
		return makeErrorResponse(http.StatusNotFound, "unavailable", "the Prometheus search API is not supported by LogicMonitor")

	default:
		return makeErrorResponse(http.StatusNotFound, "bad_data", fmt.Sprintf("unsupported resource path: %s", cleanPath))
	}
}

func makeHTTPResponse(statusCode int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func (b *Bridge) upstreamErrorResponse(msg string, err error, args ...any) (*http.Response, error) {
	lmErr := AsError(err)
	if b.log != nil {
		args = append(args, "errorType", lmErr.Type, "status", lmErr.Status, "upstreamStatus", lmErr.UpstreamStatus, "err", err)
		if lmErr.Err != nil {
			args = append(args, "cause", lmErr.Err.Error())
		}
		b.log.Error(msg, args...)
	}
	return makeErrorResponse(lmErr.Status, lmErr.Type, lmErr.Message)
}

func makeErrorResponse(statusCode int, errorType, message string) (*http.Response, error) {
	resp := PromResponse{
		Status:    "error",
		ErrorType: errorType,
		Error:     message,
	}
	body, _ := json.Marshal(resp)
	return makeHTTPResponse(statusCode, body), nil
}

func parseInstantQueryRequest(req *backend.CallResourceRequest) (query, timeStr string) {
	parsedURL, err := url.Parse(req.URL)
	if err == nil {
		q := parsedURL.Query()
		query = q.Get("query")
		timeStr = q.Get("time")
	}

	if (query == "" || timeStr == "") && len(req.Body) > 0 {
		trimmed := bytes.TrimSpace(req.Body)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			var bodyMap map[string]interface{}
			if err := json.Unmarshal(trimmed, &bodyMap); err == nil {
				if q, ok := bodyMap["query"].(string); ok && query == "" {
					query = q
				}
				if t, ok := bodyMap["time"]; ok && timeStr == "" {
					switch tv := t.(type) {
					case string:
						timeStr = tv
					case float64:
						timeStr = strconv.FormatFloat(tv, 'f', -1, 64)
					}
				}
			}
		} else {
			if vals, err := url.ParseQuery(string(req.Body)); err == nil {
				if q := vals.Get("query"); q != "" && query == "" {
					query = q
				}
				if t := vals.Get("time"); t != "" && timeStr == "" {
					timeStr = t
				}
			}
		}
	}

	return query, timeStr
}

func parseRangeQueryRequest(req *backend.CallResourceRequest) (query, start, end, step string) {
	parsedURL, err := url.Parse(req.URL)
	if err == nil {
		q := parsedURL.Query()
		query = q.Get("query")
		start = q.Get("start")
		end = q.Get("end")
		step = q.Get("step")
	}

	if (query == "" || start == "" || end == "" || step == "") && len(req.Body) > 0 {
		trimmed := bytes.TrimSpace(req.Body)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			var bodyMap map[string]string
			if err := json.Unmarshal(trimmed, &bodyMap); err == nil {
				if v, ok := bodyMap["query"]; ok && query == "" {
					query = v
				}
				if v, ok := bodyMap["start"]; ok && start == "" {
					start = v
				}
				if v, ok := bodyMap["end"]; ok && end == "" {
					end = v
				}
				if v, ok := bodyMap["step"]; ok && step == "" {
					step = v
				}
			}
		} else {
			if vals, err := url.ParseQuery(string(req.Body)); err == nil {
				if v := vals.Get("query"); v != "" && query == "" {
					query = v
				}
				if v := vals.Get("start"); v != "" && start == "" {
					start = v
				}
				if v := vals.Get("end"); v != "" && end == "" {
					end = v
				}
				if v := vals.Get("step"); v != "" && step == "" {
					step = v
				}
			}
		}
	}

	return query, start, end, step
}

func parseRequestParams(req *backend.CallResourceRequest) (int64, int64, int, []string, string) {
	var start, end int64
	limit := 200
	var matches []string
	explicitMetric := ""

	parsedURL, err := url.Parse(req.URL)
	if err == nil {
		q := parsedURL.Query()
		if s := q.Get("start"); s != "" {
			if v, ok := parseUnixSeconds(s); ok {
				start = v
			}
		}
		if e := q.Get("end"); e != "" {
			if v, ok := parseUnixSeconds(e); ok {
				end = v
			}
		}
		if l := q.Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 {
				limit = v
			}
		}
		if m := q["match[]"]; len(m) > 0 {
			matches = append(matches, m...)
		}
		if m := q["match"]; len(m) > 0 {
			matches = append(matches, m...)
		}
		if met := q.Get("metric"); met != "" {
			explicitMetric = met
		}
	}

	if len(req.Body) > 0 {
		trimmed := bytes.TrimSpace(req.Body)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			var jsonMap map[string]interface{}
			if json.Unmarshal(trimmed, &jsonMap) == nil {
				if val, ok := jsonMap["start"]; ok && start == 0 {
					switch v := val.(type) {
					case float64:
						start = int64(v)
					case string:
						if parsed, ok := parseUnixSeconds(v); ok {
							start = parsed
						}
					}
				}
				if val, ok := jsonMap["end"]; ok && end == 0 {
					switch v := val.(type) {
					case float64:
						end = int64(v)
					case string:
						if parsed, ok := parseUnixSeconds(v); ok {
							end = parsed
						}
					}
				}
				if val, ok := jsonMap["limit"]; ok {
					switch v := val.(type) {
					case float64:
						if int(v) > 0 {
							limit = int(v)
						}
					case string:
						if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
							limit = parsed
						}
					}
				}
				if val, ok := jsonMap["metric"]; ok && explicitMetric == "" {
					if s, ok := val.(string); ok && s != "" {
						explicitMetric = s
					}
				}
				for _, key := range []string{"match[]", "match"} {
					if val, ok := jsonMap[key]; ok {
						switch v := val.(type) {
						case string:
							matches = append(matches, v)
						case []interface{}:
							for _, item := range v {
								if s, ok := item.(string); ok {
									matches = append(matches, s)
								}
							}
						}
					}
				}
			}
		} else {
			if form, err := url.ParseQuery(string(req.Body)); err == nil {
				if s := form.Get("start"); s != "" && start == 0 {
					if parsed, ok := parseUnixSeconds(s); ok {
						start = parsed
					}
				}
				if e := form.Get("end"); e != "" && end == 0 {
					if parsed, ok := parseUnixSeconds(e); ok {
						end = parsed
					}
				}
				if l := form.Get("limit"); l != "" {
					if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
						limit = parsed
					}
				}
				if m := form["match[]"]; len(m) > 0 {
					matches = append(matches, m...)
				}
				if m := form["match"]; len(m) > 0 {
					matches = append(matches, m...)
				}
				if met := form.Get("metric"); met != "" && explicitMetric == "" {
					explicitMetric = met
				}
			}
		}
	}

	return start, end, limit, matches, explicitMetric
}

func parseUnixSeconds(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return int64(f), true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Unix(), true
	}
	return 0, false
}

func parseMatcherSets(matches []string) [][]*labels.Matcher {
	p := parser.NewParser(parser.Options{})
	var sets [][]*labels.Matcher
	for _, m := range matches {
		matchers, err := p.ParseMetricSelector(m)
		if err != nil {
			continue
		}
		sets = append(sets, matchers)
	}
	return sets
}

func extractMetricFromMatches(matches []string) string {
	for _, set := range parseMatcherSets(matches) {
		for _, m := range set {
			if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual && m.Value != "" {
				return m.Value
			}
		}
	}
	return ""
}

func extractLabelFilters(matches []string) map[string]LabelMatcher {
	sets := parseMatcherSets(matches)
	if len(sets) != 1 {
		return nil
	}
	filters := make(map[string]LabelMatcher)
	for _, m := range sets[0] {
		// LogicMonitor /metrics/labels and /metrics/label-values reject empty filter values (HTTP 400).
		if m.Name == model.MetricNameLabel || m.Value == "" {
			continue
		}
		var op string
		switch m.Type {
		case labels.MatchEqual:
			op = "="
		case labels.MatchNotEqual:
			op = "!="
		case labels.MatchRegexp:
			op = "=~"
		case labels.MatchNotRegexp:
			op = "!~"
		default:
			continue
		}
		filters[m.Name] = LabelMatcher{Op: op, Values: []string{m.Value}}
	}
	if len(filters) == 0 {
		return nil
	}
	return filters
}

func extractEqualsFilters(matches []string) map[string][]string {
	sets := parseMatcherSets(matches)
	if len(sets) != 1 {
		return nil
	}
	filters := make(map[string][]string)
	for _, m := range sets[0] {
		if m.Name == model.MetricNameLabel || m.Type != labels.MatchEqual || m.Value == "" {
			continue
		}
		filters[m.Name] = append(filters[m.Name], m.Value)
	}
	if len(filters) == 0 {
		return nil
	}
	return filters
}

func extractContainsFromMatches(matches []string) string {
	sets := parseMatcherSets(matches)
	if len(sets) != 1 {
		return ""
	}
	for _, m := range sets[0] {
		if m.Name != model.MetricNameLabel {
			continue
		}
		switch m.Type {
		case labels.MatchEqual:
			return m.Value
		case labels.MatchRegexp:
			if literal, ok := regexLiteral(m.Value); ok {
				return literal
			}
		}
	}
	return ""
}

func regexLiteral(expr string) (string, bool) {
	expr = strings.TrimPrefix(expr, ".*")
	expr = strings.TrimSuffix(expr, ".*")
	if expr == "" {
		return "", false
	}
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil || re.Op != syntax.OpLiteral || re.Flags&syntax.FoldCase != 0 {
		return "", false
	}
	return string(re.Rune), true
}

func filterMetricNames(translated []byte, matches []string) ([]byte, error) {
	sets := parseMatcherSets(matches)
	if len(sets) == 0 {
		return translated, nil
	}

	var resp struct {
		Status   string   `json:"status"`
		Data     []string `json:"data"`
		Warnings []string `json:"warnings,omitempty"`
	}
	if err := json.Unmarshal(translated, &resp); err != nil {
		return nil, fmt.Errorf("decode metric names: %w", err)
	}

	filtered := make([]string, 0, len(resp.Data))
	for _, name := range resp.Data {
		if nameMatchesAnySet(name, sets) {
			filtered = append(filtered, name)
		}
	}

	return json.Marshal(PromMetaResponse{
		Status:   resp.Status,
		Data:     filtered,
		Warnings: resp.Warnings,
	})
}

func nameMatchesAnySet(name string, sets [][]*labels.Matcher) bool {
	for _, set := range sets {
		matched := true
		for _, m := range set {
			if m.Name == model.MetricNameLabel && !m.Matches(name) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func (b *Bridge) fetchLabels(ctx context.Context, start, end int64, matches []string, explicitMetric string) ([]byte, error) {
	if explicitMetric == "" && len(matches) > 1 {
		return b.mergeMetadataMatches(ctx, matches, 0, func(ctx context.Context, single []string) ([]byte, error) {
			return b.fetchLabels(ctx, start, end, single, "")
		})
	}
	metric := explicitMetric
	if metric == "" {
		metric = extractMetricFromMatches(matches)
	}
	return b.client.Labels(ctx, &LabelsRequest{
		Metric:  metric,
		Start:   start,
		End:     end,
		Filters: extractLabelFilters(matches),
	})
}

func (b *Bridge) fetchLabelValues(ctx context.Context, labelName string, start, end int64, limit int, matches []string, explicitMetric string) ([]byte, error) {
	if explicitMetric == "" && len(matches) > 1 {
		return b.mergeMetadataMatches(ctx, matches, limit, func(ctx context.Context, single []string) ([]byte, error) {
			return b.fetchLabelValues(ctx, labelName, start, end, limit, single, "")
		})
	}
	metric := explicitMetric
	if metric == "" {
		metric = extractMetricFromMatches(matches)
	}
	return b.client.LabelValues(ctx, &LabelValuesRequest{
		Key:     labelName,
		Metric:  metric,
		Start:   start,
		End:     end,
		Limit:   limit,
		Filters: extractLabelFilters(matches),
	})
}

func (b *Bridge) fetchMetricNames(ctx context.Context, start, end int64, limit int, matches []string, explicitMetric string) ([]byte, error) {
	contains := explicitMetric
	if contains == "" {
		contains = extractContainsFromMatches(matches)
	}
	return b.client.MetricNames(ctx, &MetricNamesRequest{
		Start:    start,
		End:      end,
		Limit:    limit,
		Contains: contains,
		Filters:  extractEqualsFilters(matches),
	})
}

func (b *Bridge) mergeMetadataMatches(ctx context.Context, matches []string, limit int, call func(context.Context, []string) ([]byte, error)) ([]byte, error) {
	type matchResult struct {
		values  []string
		warning string
	}
	results, err := runChunks(ctx, b.client.cfg.queryRangeWorkers(), len(matches), func(ctx context.Context, i int) (matchResult, error) {
		rawResp, err := call(ctx, []string{matches[i]})
		if err != nil {
			return matchResult{}, err
		}
		data, _, warning, err := extractLMData(rawResp)
		if err != nil {
			return matchResult{}, err
		}
		var values []string
		if err := json.Unmarshal(data, &values); err != nil {
			return matchResult{}, err
		}
		return matchResult{values: values, warning: warning}, nil
	})
	if err != nil {
		return nil, err
	}

	merged := make([]string, 0)
	seen := make(map[string]struct{})
	var allWarnings []string
	seenAllWarnings := make(map[string]struct{})
	var nonEmptyWarnings []string
	seenNonEmptyWarnings := make(map[string]struct{})
	for _, res := range results {
		if res.warning != "" {
			if _, ok := seenAllWarnings[res.warning]; !ok {
				seenAllWarnings[res.warning] = struct{}{}
				allWarnings = append(allWarnings, res.warning)
			}
			if len(res.values) > 0 {
				if _, ok := seenNonEmptyWarnings[res.warning]; !ok {
					seenNonEmptyWarnings[res.warning] = struct{}{}
					nonEmptyWarnings = append(nonEmptyWarnings, res.warning)
				}
			}
		}
		for _, v := range res.values {
			if _, ok := seen[v]; !ok {
				seen[v] = struct{}{}
				merged = append(merged, v)
			}
		}
	}
	warnings := allWarnings
	if len(merged) > 0 {
		warnings = nonEmptyWarnings
	}
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}

	dataBytes, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	resp := lmAPIResponse{Data: dataBytes}
	if len(warnings) > 0 {
		resp.Meta = map[string]interface{}{"message": strings.Join(warnings, "; ")}
	}
	return json.Marshal(resp)
}

func (b *Bridge) querySeries(ctx context.Context, matches []string, end int64, limit int) (*http.Response, error) {
	if len(matches) == 0 {
		return makeErrorResponse(http.StatusBadRequest, "bad_data", "no match[] parameter provided")
	}
	if end == 0 {
		end = time.Now().Unix()
	}

	var results [][]lmMetricResult
	if limit > 0 {
		for _, match := range matches {
			rawResp, err := b.client.Query(ctx, &QueryRequest{Query: match, Time: end})
			if err != nil {
				return b.upstreamErrorResponse("failed to fetch series from LM", err, "match", match)
			}
			data, _, _, err := extractLMData(rawResp)
			if err != nil {
				return makeErrorResponse(http.StatusInternalServerError, "internal", err.Error())
			}
			metrics, err := decodeMetricResults(data)
			if err != nil {
				return makeErrorResponse(http.StatusInternalServerError, "internal", err.Error())
			}
			results = append(results, metrics)
			if len(metrics) >= limit {
				break
			}
		}
	} else {
		var err error
		results, err = runChunks(ctx, b.client.cfg.queryRangeWorkers(), len(matches), func(ctx context.Context, i int) ([]lmMetricResult, error) {
			match := matches[i]
			rawResp, err := b.client.Query(ctx, &QueryRequest{Query: match, Time: end})
			if err != nil {
				return nil, err
			}

			data, _, _, err := extractLMData(rawResp)
			if err != nil {
				return nil, err
			}
			return decodeMetricResults(data)
		})
		if err != nil {
			return b.upstreamErrorResponse("failed to fetch series from LM", err)
		}
	}

	series := make([]map[string]string, 0)
	seen := make(map[string]struct{})
	for _, metrics := range results {
		for _, metric := range metrics {
			if limit > 0 && len(series) >= limit {
				break
			}
			key := metricKey(metric.Metric)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			labelSet := metric.Metric
			if labelSet == nil {
				labelSet = map[string]string{}
			}
			series = append(series, labelSet)
		}
		if limit > 0 && len(series) >= limit {
			break
		}
	}

	body, err := json.Marshal(PromMetaResponse{Status: "success", Data: series})
	if err != nil {
		return makeErrorResponse(http.StatusInternalServerError, "internal", err.Error())
	}
	return makeHTTPResponse(http.StatusOK, body), nil
}
