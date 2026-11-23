package azopenai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"oss.nandlabs.io/golly/genai"
)

// Sentinel errors. An *APIError matches the relevant sentinel via errors.Is,
// so callers can branch on the failure class without inspecting status codes.
var (
	// ErrBadRequest is matched by HTTP 400 / 422 responses.
	ErrBadRequest = errors.New("azopenai: bad request")
	// ErrUnauthorized is matched by HTTP 401 (bad key / token).
	ErrUnauthorized = errors.New("azopenai: unauthorized")
	// ErrForbidden is matched by HTTP 403 (missing RBAC role, network rules).
	ErrForbidden = errors.New("azopenai: forbidden")
	// ErrDeploymentNotFound is matched by HTTP 404 (unknown deployment / resource).
	ErrDeploymentNotFound = errors.New("azopenai: deployment not found")
	// ErrRateLimited is matched by HTTP 429 (TPM / RPM quota exceeded).
	ErrRateLimited = errors.New("azopenai: rate limited")
	// ErrContentFiltered is matched when Azure AI Content Safety blocks the
	// prompt (error code "content_filter").
	ErrContentFiltered = errors.New("azopenai: content filtered")
	// ErrContextLengthExceeded is matched when the prompt exceeds the model's
	// context window (error code "context_length_exceeded").
	ErrContextLengthExceeded = errors.New("azopenai: context length exceeded")
	// ErrServiceUnavailable is matched by HTTP 5xx and 408 responses.
	ErrServiceUnavailable = errors.New("azopenai: service unavailable")
)

// Compile-time check: APIError carries an authoritative retry signal for the
// golly model router's fallover logic.
var _ genai.RetryableError = (*APIError)(nil)

// APIError is the typed error returned for non-2xx Azure OpenAI responses.
type APIError struct {
	// Op is the operation that failed (e.g. "chat completion", "embeddings").
	Op string
	// Deployment is the Azure deployment name the request targeted.
	Deployment string
	// StatusCode is the HTTP status code.
	StatusCode int
	// Code is the service error code (e.g. "content_filter", "DeploymentNotFound").
	Code string
	// Type is the service error type, when present.
	Type string
	// Param is the offending request parameter, when present.
	Param string
	// Message is the service error message.
	Message string

	err error
}

// Error implements error. The message embeds "status <code>" so golly's
// heuristic classifier agrees with Retryable even when the type is lost.
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "azopenai %s failed", e.Op)
	if e.Deployment != "" {
		fmt.Fprintf(&b, " (deployment %q)", e.Deployment)
	}
	fmt.Fprintf(&b, ": status %d", e.StatusCode)
	if text := http.StatusText(e.StatusCode); text != "" {
		fmt.Fprintf(&b, " %s", text)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " [%s]", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	return b.String()
}

// Unwrap returns the underlying *openai.Error.
func (e *APIError) Unwrap() error { return e.err }

// Retryable reports whether a different attempt / model may succeed:
// rate limits, timeouts and server errors are retryable; client errors,
// auth failures and content-filter blocks are not. Implements
// genai.RetryableError.
func (e *APIError) Retryable() bool {
	if e.isContentFilter() {
		return false
	}
	switch {
	case e.StatusCode == http.StatusTooManyRequests,
		e.StatusCode == http.StatusRequestTimeout,
		e.StatusCode >= 500:
		return true
	default:
		return false
	}
}

// Is matches the package sentinel errors.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrContentFiltered:
		return e.isContentFilter()
	case ErrContextLengthExceeded:
		return strings.EqualFold(e.Code, "context_length_exceeded")
	case ErrBadRequest:
		return e.StatusCode == http.StatusBadRequest || e.StatusCode == http.StatusUnprocessableEntity
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrDeploymentNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrRateLimited:
		return e.StatusCode == http.StatusTooManyRequests
	case ErrServiceUnavailable:
		return e.StatusCode >= 500 || e.StatusCode == http.StatusRequestTimeout
	}
	return false
}

func (e *APIError) isContentFilter() bool {
	code := strings.ToLower(e.Code)
	return code == "content_filter" || code == "responsibleaipolicyviolation"
}

// mapError converts SDK errors into *APIError (HTTP failures) or a wrapped
// error with operation context. Context cancellation is preserved for
// errors.Is checks.
func mapError(op, deployment string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return &APIError{
			Op:         op,
			Deployment: deployment,
			StatusCode: apiErr.StatusCode,
			Code:       apiErr.Code,
			Type:       apiErr.Type,
			Param:      apiErr.Param,
			Message:    apiErr.Message,
			err:        apiErr,
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("azopenai %s canceled: %w", op, err)
	}
	return fmt.Errorf("azopenai %s failed: %w", op, err)
}
