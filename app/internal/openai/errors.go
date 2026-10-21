package openai

import (
	"encoding/json"
	"net/http"
)

// Error codes used by the gateway. They follow OpenAI's vocabulary where one
// exists and stay stable for clients that branch on them.
const (
	CodeInvalidRequest      = "invalid_request_error"
	CodeModelNotFound       = "model_not_found"
	CodeMethodNotAllowed    = "method_not_allowed"
	CodeInvalidAPIKey       = "invalid_api_key"
	CodeUpstreamUnavailable = "upstream_unavailable"
	CodeUpstreamError       = "upstream_error"
	CodeUpstreamIdle        = "upstream_idle_timeout"
	CodeUpstreamStream      = "upstream_stream_error"
	CodeUpstreamAuth        = "upstream_auth_failed"
	CodeUpstreamClearance   = "upstream_clearance_failed"
	CodeRequestTooLarge     = "request_too_large"
)

// RequestError is a client-facing error with its HTTP status.
type RequestError struct {
	Status  int
	Message string
	Type    string
	Code    string
	Param   string
}

// Error implements error.
func (e *RequestError) Error() string { return e.Message }

// Invalid returns a 400 invalid_request_error.
func Invalid(message string) *RequestError {
	return &RequestError{Status: http.StatusBadRequest, Message: message, Type: "invalid_request_error", Code: CodeInvalidRequest}
}

// New builds a RequestError with explicit fields.
func New(status int, errType, code, param, message string) *RequestError {
	return &RequestError{Status: status, Message: message, Type: errType, Code: code, Param: param}
}

// Envelope renders e as an OpenAI error response body.
func (e *RequestError) Envelope() ErrorResponse {
	return ErrorResponse{Error: ErrorBody{
		Message: e.Message,
		Type:    e.Type,
		Code:    e.Code,
		Param:   e.Param,
	}}
}

// WriteError writes e as an OpenAI-shaped JSON error response.
func WriteError(w http.ResponseWriter, e *RequestError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e.Envelope())
}
