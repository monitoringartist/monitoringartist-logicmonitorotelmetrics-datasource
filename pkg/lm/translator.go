package lm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type PromResponse struct {
	Status    string   `json:"status"`
	Data      PromData `json:"data"`
	Warnings  []string `json:"warnings,omitempty"`
	ErrorType string   `json:"errorType,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type PromData struct {
	ResultType string      `json:"resultType"`
	Result     interface{} `json:"result"`
}

type PromMetaResponse struct {
	Status    string      `json:"status"`
	Data      interface{} `json:"data"`
	Warnings  []string    `json:"warnings,omitempty"`
	ErrorType string      `json:"errorType,omitempty"`
	Error     string      `json:"error,omitempty"`
}

type MetricMetadata struct {
	Type string `json:"type"`
	Help string `json:"help"`
	Unit string `json:"unit"`
}

func extractLMData(rawBody []byte) (json.RawMessage, string, string, error) {
	if !json.Valid(rawBody) {
		return nil, "", "", fmt.Errorf("LogicMonitor response is not valid JSON")
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &object); err != nil {
		return nil, "", "", fmt.Errorf("decode LogicMonitor response: %w", err)
	}

	var meta struct {
		ResultType string `json:"resultType"`
		Message    string `json:"message"`
	}
	if rawMeta, ok := object["meta"]; ok {
		if err := json.Unmarshal(rawMeta, &meta); err != nil {
			return nil, "", "", fmt.Errorf("decode LogicMonitor response metadata: %w", err)
		}
	}

	data, hasData := object["data"]
	if !hasData || len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if strings.TrimSpace(meta.Message) != "" {
			return json.RawMessage("[]"), meta.ResultType, meta.Message, nil
		}
		if !hasData {
			return json.RawMessage(bytes.TrimSpace(rawBody)), meta.ResultType, "", nil
		}
		return json.RawMessage("[]"), meta.ResultType, meta.Message, nil
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var promData struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(trimmed, &promData); err == nil && promData.Result != nil {
			resultType := meta.ResultType
			if promData.ResultType != "" {
				resultType = promData.ResultType
			}
			return promData.Result, resultType, meta.Message, nil
		}
	}

	return data, meta.ResultType, meta.Message, nil
}

func (v *lmValue) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	var rawTimestamp, rawValue json.RawMessage
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var pair []json.RawMessage
		if err := json.Unmarshal(trimmed, &pair); err != nil {
			return err
		}
		if len(pair) != 2 {
			return fmt.Errorf("expected [timestamp, value] pair, got %d elements", len(pair))
		}
		rawTimestamp, rawValue = pair[0], pair[1]
	} else {
		var obj struct {
			Timestamp json.RawMessage `json:"timestamp"`
			Value     json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return err
		}
		rawTimestamp, rawValue = obj.Timestamp, obj.Value
	}

	timestamp, err := jsonNumberString(rawTimestamp)
	if err != nil {
		return fmt.Errorf("invalid timestamp: %w", err)
	}
	ts, err := strconv.ParseFloat(timestamp, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp %q: %w", timestamp, err)
	}
	if ts > 1e11 {
		ts = ts / 1000
	}

	value, err := jsonNumberString(rawValue)
	if err != nil {
		return fmt.Errorf("invalid value: %w", err)
	}

	v.Timestamp = ts
	v.Value = value
	return nil
}

func jsonNumberString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("missing")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", err
	}
	return n.String(), nil
}

func decodeMetricResults(data json.RawMessage) ([]lmMetricResult, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("expected LogicMonitor metric data to be an array")
	}

	var metrics []lmMetricResult
	if err := json.Unmarshal(trimmed, &metrics); err != nil {
		return nil, fmt.Errorf("decode LogicMonitor metric data: %w", err)
	}
	if metrics == nil {
		return nil, fmt.Errorf("LogicMonitor metric data is null; expected an array")
	}

	return metrics, nil
}

// TranslateLMResponse translates a LogicMonitor metric query response into Prometheus format.
func TranslateLMResponse(rawLMBody []byte, defaultResultType string) ([]byte, error) {
	if !json.Valid(rawLMBody) {
		return nil, fmt.Errorf("LogicMonitor response is not valid JSON")
	}

	data, upstreamResultType, warning, err := extractLMData(rawLMBody)
	if err != nil {
		return nil, err
	}

	resultType := defaultResultType
	if upstreamResultType != "" {
		resultType = upstreamResultType
	}

	if resultType == "scalar" || resultType == "string" {
		var value lmValue
		if err := json.Unmarshal(data, &value); err == nil {
			return json.Marshal(PromResponse{
				Status:   "success",
				Data:     PromData{ResultType: resultType, Result: []interface{}{value.Timestamp, value.Value}},
				Warnings: nonEmptyWarnings(warning),
			})
		}
	}

	metrics, err := decodeMetricResults(data)
	if err != nil {
		return nil, err
	}

	var promResult interface{}

	switch resultType {
	case "scalar":
		if len(metrics) == 0 {
			return nil, fmt.Errorf("LogicMonitor scalar response contains no metric values")
		}

		var value *lmValue
		if len(metrics[0].Values) > 0 {
			value = &metrics[0].Values[0]
		} else {
			value = metrics[0].Value
		}
		if value == nil {
			return nil, fmt.Errorf("LogicMonitor scalar response contains no value")
		}
		promResult = []interface{}{value.Timestamp, value.Value}

	case "vector":
		vectorResults := make([]map[string]interface{}, 0, len(metrics))
		for i, metric := range metrics {
			var value *lmValue
			if len(metric.Values) > 0 {
				value = &metric.Values[0]
			} else {
				value = metric.Value
			}
			if value == nil {
				return nil, fmt.Errorf("LogicMonitor vector result %d contains no value", i)
			}

			mLabels := metric.Metric
			if mLabels == nil {
				mLabels = map[string]string{}
			}
			vectorResults = append(vectorResults, map[string]interface{}{
				"metric": mLabels,
				"value":  []interface{}{value.Timestamp, value.Value},
			})
		}
		promResult = vectorResults

	case "matrix":
		matrixResults := make([]map[string]interface{}, 0, len(metrics))
		for i, metric := range metrics {
			if metric.Values == nil {
				return nil, fmt.Errorf("LogicMonitor matrix result %d is missing values", i)
			}

			formattedValues := make([][]interface{}, 0, len(metric.Values))
			for _, value := range metric.Values {
				formattedValues = append(formattedValues, []interface{}{value.Timestamp, value.Value})
			}
			mLabels := metric.Metric
			if mLabels == nil {
				mLabels = map[string]string{}
			}
			matrixResults = append(matrixResults, map[string]interface{}{
				"metric": mLabels,
				"values": formattedValues,
			})
		}
		promResult = matrixResults

	default:
		return nil, fmt.Errorf("unsupported Prometheus result type %q", resultType)
	}

	promResp := PromResponse{
		Status: "success",
		Data: PromData{
			ResultType: resultType,
			Result:     promResult,
		},
		Warnings: nonEmptyWarnings(warning),
	}

	return json.Marshal(promResp)
}

// TranslateMetadataResponse translates LogicMonitor string array metadata responses to Prometheus metadata format.
func TranslateMetadataResponse(rawBody []byte) ([]byte, error) {
	if !json.Valid(rawBody) {
		return nil, fmt.Errorf("LogicMonitor response is not valid JSON")
	}

	data, _, warning, err := extractLMData(rawBody)
	if err != nil {
		return nil, err
	}

	var rawData interface{}
	if err := json.Unmarshal(data, &rawData); err != nil {
		return nil, fmt.Errorf("decode LogicMonitor metadata data: %w", err)
	}
	if rawData == nil {
		return nil, fmt.Errorf("LogicMonitor metadata data is null")
	}

	resp := PromMetaResponse{
		Status:   "success",
		Data:     rawData,
		Warnings: nonEmptyWarnings(warning),
	}

	return json.Marshal(resp)
}

// TranslateSeriesResponse translates LogicMonitor metric-names responses to Prometheus series format.
func TranslateSeriesResponse(rawBody []byte) ([]byte, error) {
	if !json.Valid(rawBody) {
		return nil, fmt.Errorf("LogicMonitor response is not valid JSON")
	}

	data, _, warning, err := extractLMData(rawBody)
	if err != nil {
		return nil, err
	}

	var metricNames []string
	if err := json.Unmarshal(data, &metricNames); err != nil {
		return nil, fmt.Errorf("decode LogicMonitor series data: %w", err)
	}

	seriesList := make([]map[string]string, 0, len(metricNames))
	for _, name := range metricNames {
		seriesList = append(seriesList, map[string]string{
			"__name__": name,
		})
	}

	resp := PromMetaResponse{
		Status:   "success",
		Data:     seriesList,
		Warnings: nonEmptyWarnings(warning),
	}

	return json.Marshal(resp)
}

// TranslateMetricMetadataResponse translates LogicMonitor metric-names responses to Prometheus metadata map format.
func TranslateMetricMetadataResponse(rawBody []byte) ([]byte, error) {
	if !json.Valid(rawBody) {
		return nil, fmt.Errorf("LogicMonitor response is not valid JSON")
	}

	data, _, warning, err := extractLMData(rawBody)
	if err != nil {
		return nil, err
	}

	var metricNames []string
	if err := json.Unmarshal(data, &metricNames); err != nil {
		metricNames = []string{}
	}

	metadataMap := make(map[string][]MetricMetadata)
	for _, m := range metricNames {
		metadataMap[m] = []MetricMetadata{
			{
				Type: "gauge",
				Help: "Metric exported from LogicMonitor OpenTelemetry",
				Unit: "",
			},
		}
	}

	resp := PromMetaResponse{
		Status:   "success",
		Data:     metadataMap,
		Warnings: nonEmptyWarnings(warning),
	}

	return json.Marshal(resp)
}

func nonEmptyWarnings(warning string) []string {
	if warning == "" {
		return nil
	}
	return []string{warning}
}
