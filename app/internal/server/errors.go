package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// upstreamError is the one upstream → OpenAI envelope mapper (§6.1). Every
// non-streaming route calls it: chat, web_search, speech and transcription.
// A per-family mapper is how the two search codes and a duplicate
// upstream_errors increment drifted in an earlier pass.
//
// It is used only before any bytes are written; streamError is the mid-stream
// variant of the same table.
func (s *Server) upstreamError(err error) *openai.RequestError {
	s.upErrors.Add(1)
	switch {
	case errors.Is(err, upstream.ErrIdleTimeout):
		return openai.New(http.StatusGatewayTimeout, "upstream_error", openai.CodeUpstreamIdle, "",
			"upstream sent nothing for the idle window; the request was abandoned")
	case errors.Is(err, upstream.ErrStreamTruncated), errors.Is(err, upstream.ErrSearchIncomplete):
		return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamStream, "",
			"upstream closed the stream before completion")
	case errors.Is(err, upstream.ErrClearance):
		// Retryable from the client's point of view: the upstream is reachable
		// but this gateway cannot prove itself right now.
		return openai.New(http.StatusServiceUnavailable, "server_error", openai.CodeUpstreamClearance, "",
			"the upstream bot checkpoint could not be cleared; check the egress and /healthz clearance state")
	case errors.Is(err, upstream.ErrUnavailable):
		return openai.New(http.StatusServiceUnavailable, "server_error", openai.CodeUpstreamUnavailable, "",
			"upstream session pool is empty; the upstream may be challenging this egress or unavailable, retry shortly")
	case errors.Is(err, upstream.ErrAuthRejected):
		return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamAuth, "",
			"upstream rejected newly acquired session tokens")
	case errors.Is(err, context.Canceled):
		return openai.New(http.StatusRequestTimeout, "invalid_request_error", openai.CodeInvalidRequest, "",
			"request canceled by the client")
	case errors.Is(err, errUpstreamEmpty):
		return upstreamEmpty()
	}
	var se *upstream.StatusError
	if errors.As(err, &se) {
		s.logf("upstream: request failed: status %d body %q", se.Status, truncate(se.Body, 300))
		// Translate, do not second-guess: the upstream's own message rides in
		// the OpenAI envelope unchanged (minus HTML challenge pages).
		msg := fmt.Sprintf("upstream returned HTTP %d", se.Status)
		if m := se.Message(); m != "" {
			msg = m
		}
		return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamError, "", msg)
	}
	var searchErr *upstream.SearchError
	if errors.As(err, &searchErr) {
		s.logf("upstream: search failed: %s", truncate(searchErr.Message, 300))
		msg := "upstream search pipeline failed"
		if m := searchErr.Message; m != "" {
			msg = truncate(m, 1000)
		}
		return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamError, "", msg)
	}
	s.logf("upstream: request failed: %v", err)
	if errors.Is(err, context.DeadlineExceeded) {
		return openai.New(http.StatusGatewayTimeout, "upstream_error", openai.CodeUpstreamIdle, "",
			"the upstream did not respond before the request deadline")
	}
	return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamError, "",
		"upstream request failed")
}

// errUpstreamEmpty marks the empty-completion failure mode: an upstream
// stream that ended without ever producing visible text. It is never
// returned by the transport; the streaming paths raise it locally when the
// read loop finishes (or fails) with no text delta seen, so the client gets
// a real HTTP 502 instead of a 200 with an empty SSE body.
var errUpstreamEmpty = fmt.Errorf("upstream: empty response")

// upstreamEmpty maps the empty-completion error to its client envelope.
func upstreamEmpty() *openai.RequestError {
	return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamEmpty, "",
		"upstream returned an empty response")
}

// streamError maps a mid-stream read failure to an SSE error payload. Same
// table as upstreamError, different output shape (§3.2).
func streamError(err error) openai.RequestError {
	switch {
	case errors.Is(err, upstream.ErrIdleTimeout):
		return *openai.New(http.StatusGatewayTimeout, "upstream_error", openai.CodeUpstreamIdle, "",
			"upstream sent nothing for the idle window; the generation was abandoned")
	case errors.Is(err, upstream.ErrStreamTruncated):
		return *openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamStream, "",
			"upstream closed the stream before completion")
	case errors.Is(err, context.Canceled):
		return *openai.New(499, "upstream_error", openai.CodeUpstreamStream, "",
			"client disconnected")
	default:
		return *openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamStream, "",
			"upstream stream failed")
	}
}

// methodNotAllowed writes the uniform 405 (§3.4): one code, one message, and
// an Allow header naming the one method the route serves.
func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	openai.WriteError(w, openai.New(http.StatusMethodNotAllowed, "invalid_request_error", openai.CodeMethodNotAllowed, "",
		"method not allowed; use "+allow))
}

// readJSONBody applies the shared request-body cap, reads the body, and maps
// the failure (oversize → 413, anything else → 400). It is the one body read
// for the JSON routes; transcription has its own cap and multipart parser.
func (s *Server) readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, *openai.RequestError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, openai.New(http.StatusRequestEntityTooLarge, "invalid_request_error", openai.CodeRequestTooLarge, "",
				fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes))
		}
		return nil, openai.Invalid(fmt.Sprintf("read request body: %v", err))
	}
	return body, nil
}

// decodeJSONObject is the one structural JSON reader for the request bodies:
// it enforces "one JSON object, nothing after it" (§1.1 class 1) and returns
// the raw object so the caller can apply its own field rules.
func decodeJSONObject(body []byte) (map[string]json.RawMessage, *openai.RequestError) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil {
		return nil, openai.Invalid(fmt.Sprintf("invalid JSON body: %v", err))
	}
	// Anything other than EOF after the object — valid JSON or garbage — is
	// rejected: a prefix that a front proxy or WAF might parse differently
	// must not be silently tolerated.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, openai.Invalid("invalid JSON body: unexpected data after the request object")
	}
	if raw == nil {
		return nil, openai.Invalid("request body must be a JSON object")
	}
	return raw, nil
}
