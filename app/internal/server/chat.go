package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// upstreamError maps an upstream failure to a client-facing error. It is
// used only before any bytes are written; once a stream has started,
// midStreamError is used instead.
func (s *Server) upstreamError(err error) *openai.RequestError {
	s.upErrors.Add(1)
	switch {
	case errors.Is(err, upstream.ErrIdleTimeout):
		return openai.New(http.StatusGatewayTimeout, "upstream_error", openai.CodeUpstreamIdle, "",
			"upstream sent nothing for the idle window; the generation was abandoned")
	case errors.Is(err, upstream.ErrStreamTruncated):
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
	}
	var se *upstream.StatusError
	if errors.As(err, &se) {
		s.logf("upstream: chat failed: status %d body %q", se.Status, truncate(se.Body, 300))
		return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamError, "",
			fmt.Sprintf("upstream returned HTTP %d", se.Status))
	}
	s.logf("upstream: chat failed: %v", err)
	if errors.Is(err, context.DeadlineExceeded) {
		return openai.New(http.StatusGatewayTimeout, "upstream_error", openai.CodeUpstreamIdle, "",
			"the upstream did not respond before the request deadline")
	}
	return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamError, "",
		"upstream request failed")
}

// streamError maps a mid-stream read failure to an SSE error payload.
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

// stream writes the OpenAI SSE response. Headers are committed only after
// the upstream stream is open, so pool and upstream failures still carry a
// proper HTTP status.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, ureq *upstream.ChatRequest, model string) {
	stream, err := s.up.Chat(r.Context(), ureq, s.cfg.IdleTimeout, func(line string) {
		s.logf("upstream: skipping malformed SSE line: %q", truncate(line, 200))
	})
	if err != nil {
		s.fail(w, s.upstreamError(err))
		return
	}
	defer stream.Close()
	s.streams.Add(1)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	id := upstream.RandomID("chatcmpl")
	created := s.now().Unix()
	write := func(delta openai.Delta, finish *string) bool {
		chunk := openai.Chunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
		if err := writeEvent(w, chunk); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	// Role-first frame: clients get a valid chunk immediately even when the
	// diffusion model spends the next many seconds thinking.
	if !write(openai.Delta{Role: "assistant"}, nil) {
		return
	}

	readErr := stream.Reader.Read(r.Context(), func(ev upstream.Event) error {
		switch ev.Type {
		case upstream.EventReasoningDelta:
			if text := ev.Reasoning(); text != "" && !write(openai.Delta{ReasoningContent: text}, nil) {
				return errClientGone
			}
		case upstream.EventTextStart, upstream.EventTextDelta:
			if text := ev.ContentText(); text != "" && !write(openai.Delta{Content: text}, nil) {
				return errClientGone
			}
		}
		return nil
	})

	if readErr != nil {
		if errors.Is(readErr, errClientGone) {
			return
		}
		if errors.Is(readErr, context.Canceled) && r.Context().Err() != nil {
			// Client went away: nothing to write to, and the pool token
			// stays valid.
			return
		}
		s.upErrors.Add(1)
		s.logf("upstream: stream failed after headers: %v", readErr)
		se := streamError(readErr)
		_ = writeEvent(w, se.Envelope())
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	stop := "stop"
	if !write(openai.Delta{}, &stop) {
		return
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// errClientGone aborts the read loop when writing to the client fails.
var errClientGone = errors.New("server: client write failed")

// complete aggregates the upstream stream into a single chat.completion.
func (s *Server) complete(w http.ResponseWriter, r *http.Request, ureq *upstream.ChatRequest, model string) {
	stream, err := s.up.Chat(r.Context(), ureq, s.cfg.IdleTimeout, func(line string) {
		s.logf("upstream: skipping malformed SSE line: %q", truncate(line, 200))
	})
	if err != nil {
		s.fail(w, s.upstreamError(err))
		return
	}
	defer stream.Close()

	var content, reasoning strings.Builder
	readErr := stream.Reader.Read(r.Context(), func(ev upstream.Event) error {
		switch ev.Type {
		case upstream.EventReasoningDelta:
			reasoning.WriteString(ev.Reasoning())
		case upstream.EventTextStart, upstream.EventTextDelta:
			content.WriteString(ev.ContentText())
		}
		return nil
	})
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) && r.Context().Err() != nil {
			return
		}
		s.fail(w, s.upstreamError(readErr))
		return
	}

	writeJSON(w, http.StatusOK, openai.Completion{
		ID:      upstream.RandomID("chatcmpl"),
		Object:  "chat.completion",
		Created: s.now().Unix(),
		Model:   model,
		Choices: []openai.CompletionChoice{{
			Index: 0,
			Message: openai.CompletionMessage{
				Role:             "assistant",
				Content:          content.String(),
				ReasoningContent: reasoning.String(),
			},
			FinishReason: "stop",
		}},
	})
}

// writeEvent writes one SSE data frame.
func writeEvent(w http.ResponseWriter, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
