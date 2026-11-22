package lm

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode"
)

const (
	ErrorTypeBadData         = "bad_data"
	ErrorTypeUnauthorized    = "unauthorized"
	ErrorTypeForbidden       = "forbidden"
	ErrorTypeNotFound        = "not_found"
	ErrorTypeTooManyRequests = "too_many_requests"
	ErrorTypeTimeout         = "timeout"
	ErrorTypeUnavailable     = "unavailable"
	ErrorTypeCanceled        = "canceled"
	ErrorTypeInternal        = "internal"

	statusClientClosedRequest = 499
	maxLMMessageLength        = 200
)

// MissingCredentialsError reports credentials that are not configured.
type MissingCredentialsError struct {
	Message string
}

func (e *MissingCredentialsError) Error() string {
	return e.Message
}

func missingCredentials(message string) error {
	return &MissingCredentialsError{Message: message}
}

// InvalidAccountError reports an account name that is not a bare LogicMonitor account.
type InvalidAccountError struct {
	Value string
}

func (e *InvalidAccountError) Error() string {
	return fmt.Sprintf("invalid LogicMonitor account %q: enter only the account name (e.g. \"acme\" for https://acme.logicmonitor.com), not a URL or domain", e.Value)
}

// HealthCheckTracker captures error messages returned by LogicMonitor during a health check.
type HealthCheckTracker struct {
	mu           sync.Mutex
	errorMessage string
}

// SetErrorMessage records msg in a concurrency-safe manner.
func (h *HealthCheckTracker) SetErrorMessage(msg string) {
	h.mu.Lock()
	h.errorMessage = msg
	h.mu.Unlock()
}

// ErrorMessage returns the captured error message in a concurrency-safe manner.
func (h *HealthCheckTracker) ErrorMessage() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.errorMessage
}

type healthCheckContextKey struct{}

// WithHealthCheckTracker associates a HealthCheckTracker with ctx.
func WithHealthCheckTracker(ctx context.Context, tracker *HealthCheckTracker) context.Context {
	return context.WithValue(ctx, healthCheckContextKey{}, tracker)
}

// HealthCheckTrackerFromContext extracts the HealthCheckTracker from ctx, if present.
func HealthCheckTrackerFromContext(ctx context.Context) *HealthCheckTracker {
	if tracker, ok := ctx.Value(healthCheckContextKey{}).(*HealthCheckTracker); ok {
		return tracker
	}
	return nil
}

// Error is a classified LogicMonitor request failure. Message is safe to show
// to users; it never contains raw upstream response bodies.
type Error struct {
	Type           string
	Status         int
	UpstreamStatus int
	LMMessage      string
	Message        string
	Err            error
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) Unwrap() error {
	return e.Err
}

// Downstream reports whether the failure originates outside the plugin.
func (e *Error) Downstream() bool {
	return e.Type != ErrorTypeInternal
}

// AsError classifies err into an *Error. Unclassified errors are plugin-internal.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Type: ErrorTypeTimeout, Status: http.StatusGatewayTimeout, Message: "LogicMonitor request timed out", Err: err}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Type: ErrorTypeCanceled, Status: statusClientClosedRequest, Message: "LogicMonitor request was canceled", Err: err}
	}
	return &Error{Type: ErrorTypeInternal, Status: http.StatusInternalServerError, Message: err.Error(), Err: err}
}

func badDataError(err error) *Error {
	return &Error{Type: ErrorTypeBadData, Status: http.StatusBadRequest, Message: err.Error(), Err: err}
}

func unavailableError(err error) *Error {
	return &Error{Type: ErrorTypeUnavailable, Status: http.StatusBadGateway, Message: err.Error(), Err: err}
}

func statusError(statusCode int, body []byte) *Error {
	e := &Error{UpstreamStatus: statusCode, LMMessage: lmErrorMessage(body)}
	var summary string
	switch statusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		e.Type, e.Status, summary = ErrorTypeBadData, http.StatusBadRequest, "LogicMonitor rejected the request"
	case http.StatusUnauthorized:
		e.Type, e.Status, summary = ErrorTypeUnauthorized, http.StatusUnauthorized, "LogicMonitor authentication failed"
	case http.StatusForbidden:
		e.Type, e.Status, summary = ErrorTypeForbidden, http.StatusForbidden, "LogicMonitor denied access"
	case http.StatusNotFound:
		e.Type, e.Status, summary = ErrorTypeNotFound, http.StatusNotFound, "LogicMonitor resource not found"
	case http.StatusTooManyRequests:
		e.Type, e.Status, summary = ErrorTypeTooManyRequests, http.StatusTooManyRequests, "LogicMonitor rate limit exceeded"
	case http.StatusGatewayTimeout:
		e.Type, e.Status, summary = ErrorTypeTimeout, http.StatusGatewayTimeout, "LogicMonitor API timed out"
	case http.StatusServiceUnavailable:
		e.Type, e.Status, summary = ErrorTypeUnavailable, http.StatusServiceUnavailable, "LogicMonitor API is unavailable"
	default:
		e.Type, e.Status, summary = ErrorTypeUnavailable, http.StatusBadGateway, "Unexpected response from LogicMonitor API"
	}
	e.Message = fmt.Sprintf("%s (HTTP %d)", summary, statusCode)
	if e.LMMessage != "" {
		e.Message += ": " + e.LMMessage
	}
	return e
}

func withMessage(e *Error, message string) *Error {
	e.Message = message
	if e.LMMessage != "" {
		e.Message += ": " + e.LMMessage
	}
	return e
}

func networkError(err error) *Error {
	cause := err
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		cause = urlErr.Err
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Type: ErrorTypeTimeout, Status: http.StatusGatewayTimeout, Message: "LogicMonitor API request timed out: " + cause.Error(), Err: err}
	}
	return &Error{Type: ErrorTypeUnavailable, Status: http.StatusServiceUnavailable, Message: "Could not connect to LogicMonitor API: " + cause.Error(), Err: err}
}

func lmErrorMessage(body []byte) string {
	var fields map[string]interface{}
	if json.Unmarshal(body, &fields) != nil {
		return ""
	}
	for _, key := range []string{"errorMessage", "errmsg", "message", "error"} {
		if s, ok := fields[key].(string); ok {
			if s = sanitizeMessage(s); s != "" {
				return s
			}
		}
	}
	return ""
}

func sanitizeMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > maxLMMessageLength {
		s = string(runes[:maxLMMessageLength]) + "…"
	}
	return s
}

// IsTLSError reports whether err is a TLS handshake or certificate error.
func IsTLSError(err error) bool {
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	return errors.As(err, &certErr) || errors.As(err, &unknownAuthority) || errors.As(err, &hostnameErr) ||
		errors.As(err, &invalidCert) || errors.As(err, &recordErr) || errors.As(err, &alertErr)
}

// IsProxyError reports whether err happened while connecting through an HTTP
// proxy or a SOCKS proxy such as Grafana Private Data Source Connect.
func IsProxyError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && (opErr.Op == "proxyconnect" || strings.HasPrefix(opErr.Op, "socks"))
}

// IsDNSError reports whether err is a DNS resolution failure.
func IsDNSError(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

func retryableNetworkError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false
	}
	return !IsTLSError(err) && !IsProxyError(err)
}
