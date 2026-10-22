package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/tools"
	"inceptionlabs2api/internal/upstream"
)

// toolPlan is the request-scoped state derived from tools/tool_choice.
type toolPlan struct {
	manifest *tools.Manifest
	hist     []openai.History
	// native reports that the gateway's own web_search tool was added to the
	// manifest, so its calls must be served internally rather than emitted.
	native bool
}

// planTools builds the manifest and history for a tool-bearing request. It
// returns a nil plan when the request must take the legacy path. native is the
// optional gateway-served search tool; it is added only when the client did
// not declare that name itself (a client tool always wins).
func planTools(req *openai.ChatRequest, native *tools.Tool) (*toolPlan, *openai.RequestError) {
	if len(req.Tools) == 0 || req.ToolChoice.Kind == openai.ToolChoiceNone {
		return nil, nil
	}
	list := make([]tools.Tool, 0, len(req.Tools)+1)
	for _, t := range req.Tools {
		if t.Function.Name == tools.NativeSearchName {
			native = nil
		}
		list = append(list, tools.Tool{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters})
	}
	if native != nil {
		list = append(list, *native)
	}
	choice := tools.Choice{}
	switch req.ToolChoice.Kind {
	case openai.ToolChoiceRequired:
		choice.Required = true
	case openai.ToolChoiceNamed:
		choice.Named = req.ToolChoice.Name
	default:
		choice.Auto = true
	}
	parallelOff := req.ParallelToolCalls != nil && !*req.ParallelToolCalls
	manifest, err := tools.BuildManifest(list, choice, parallelOff)
	if err != nil {
		return nil, openai.Invalid(err.Error())
	}
	hist, _, re := openai.NormalizeHistory(req.Messages)
	if re != nil {
		return nil, re
	}
	return &toolPlan{manifest: manifest, hist: hist, native: native != nil}, nil
}

// chatWithTools answers a request whose tools are active. A plain manifest
// streams; the native web_search loop resolves internally first, so a
// manifest with native search on takes the pre-buffered path.
func (s *Server) chatWithTools(w http.ResponseWriter, r *http.Request, req *openai.ChatRequest, plan *toolPlan) {
	ureq := upstream.NewToolChatRequestHistory(plan.hist, req.ReasoningEffort, plan.manifest)
	if req.Stream {
		s.streamTools(w, r, ureq, req.Model, plan.manifest, plan)
		return
	}
	s.completeTools(w, r, ureq, req.Model, plan.manifest, plan)
}

// round is one upstream response with its parsing applied.
type round struct {
	content   string
	reasoning string
	calls     []tools.Call
	cites     upstream.CitationCollector
}

// readRound opens one upstream stream and consumes it fully.
func (s *Server) readRound(ctx context.Context, ureq *upstream.ChatRequest, manifest *tools.Manifest) (*round, error) {
	stream, err := s.up.Chat(ctx, ureq, s.cfg.IdleTimeout, func(line string) {
		s.logf("upstream: skipping malformed SSE line: %q", truncate(line, 200))
	})
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	var out round
	parser := tools.NewParser(manifest.Declared)
	rtag := wireTag()
	readErr := stream.Reader.Read(ctx, func(ev upstream.Event) error {
		out.cites.Observe(ev, s.logf)
		defer wireDumpEvent(rtag, ev, 0, 0)
		switch ev.Type {
		case upstream.EventReasoningDelta:
			out.reasoning += ev.Reasoning()
		case upstream.EventTextStart, upstream.EventTextDelta:
			out.content += string(parser.Feed([]byte(ev.ContentText())))
		}
		return nil
	})
	if readErr != nil {
		return nil, readErr
	}
	out.content += string(parser.Flush())
	out.calls = parser.Calls()
	return &out, nil
}

// emptyToolAnswer reports the measured upstream failure mode: with a tool
// manifest present the model occasionally returns reasoning-only, zero text
// and zero calls (probe/outQ q1/q3/q4, probe/outP2 timing runs). The upstream
// web surface shows the same flakiness without any preamble, so this is the
// model's occasional refusal-shaped pause, not a request defect. The client
// asked for tool calling; an empty answer is not a valid tool response, so
// the gateway re-asks once internally with the same manifest — protocol
// completion, not content interception: whatever the retry returns is passed
// through verbatim, empty or not.
func (r *round) emptyToolAnswer() bool {
	return r.content == "" && r.reasoning == "" && len(r.calls) == 0
}

// emitToolCalls writes one delta frame per parsed call and returns the number
// emitted so far.
func emitToolCalls(w http.ResponseWriter, flusher http.Flusher, calls []tools.Call, emitted int, write func(openai.Delta, *string) bool) int {
	for ; emitted < len(calls); emitted++ {
		c := calls[emitted]
		delta := openai.Delta{ToolCalls: []openai.ToolCallDelta{{
			Index: emitted,
			ID:    tools.NewID(),
			Type:  "function",
			Function: openai.FunctionCallDelta{
				Name:      c.Name,
				Arguments: c.Arguments,
			},
		}}}
		if !write(delta, nil) {
			return emitted
		}
	}
	return emitted
}

// streamTools is the streaming path for a plain manifest. With the
// native search tool active it runs the internal loop first: every round is
// consumed privately (no bytes) until the model stops calling web_search, and
// only that final round is replayed as frames.
func (s *Server) streamTools(w http.ResponseWriter, r *http.Request, ureq *upstream.ChatRequest, model string, manifest *tools.Manifest, plan *toolPlan) {
	if plan.native {
		s.streamNative(w, r, ureq, model, plan)
		return
	}
	rtag := wireTag()
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
	write := s.chunkWriter(w, flusher, id, created, model, rtag)
	if !write(openai.Delta{Role: "assistant"}, nil) {
		return
	}

	var cites upstream.CitationCollector
	parser := tools.NewParser(manifest.Declared)
	emitted := 0
	// sawAlive tracks whether the round released anything past the opening
	// role frame. The measured empty-answer failure mode (reasoning-only
	// stream, no text, no calls) never does, so the retry happens before the
	// first answer frame leaves. Reasoning deltas are buffered, not written,
	// until the first content/call proves the round is alive.
	sawAlive := false
	bufReasoning := ""
	readErr := stream.Reader.Read(r.Context(), func(ev upstream.Event) error {
		cites.Observe(ev, s.logf)
		defer wireDumpEvent(rtag, ev, 0, len(parser.Calls()))
		switch ev.Type {
		case upstream.EventReasoningDelta:
			if sawAlive {
				if text := ev.Reasoning(); text != "" && !write(openai.Delta{ReasoningContent: text}, nil) {
					return errClientGone
				}
			} else {
				bufReasoning += ev.Reasoning()
			}
		case upstream.EventTextStart, upstream.EventTextDelta:
			released := parser.Feed([]byte(ev.ContentText()))
			if !sawAlive {
				// First sign of life: flush the buffered reasoning, then go
				// live. A <tool_call>-block call may release no text at all, so a
				// parsed call alone also counts as alive.
				sawAlive = true
				if bufReasoning != "" {
					if !write(openai.Delta{ReasoningContent: bufReasoning}, nil) {
						return errClientGone
					}
					bufReasoning = ""
				}
			}
			if len(released) > 0 && !write(openai.Delta{Content: string(released)}, nil) {
				return errClientGone
			}
			emitted = emitToolCalls(w, flusher, parser.Calls(), emitted, write)
			if len(parser.Calls()) > 0 {
				sawAlive = true
			}
		}
		return nil
	})
	// The measured empty-answer failure mode ends the upstream stream with
	// nothing ever released. If no byte went out (and the client is still
	// connected), re-ask ONCE with a fresh stream before writing anything.
	// The retry consumes the same upstream contract; its result passes
	// through verbatim, empty or not.
	if readErr == nil && !sawAlive && len(parser.Calls()) == 0 {
		stream.Close()
		s.logf("tools: empty answer round (no text/call), retrying once")
		stream2, err2 := s.up.Chat(r.Context(), ureq, s.cfg.IdleTimeout, func(line string) {
			s.logf("upstream: skipping malformed SSE line: %q", truncate(line, 200))
		})
		if err2 == nil {
			defer stream2.Close()
			parser2 := tools.NewParser(manifest.Declared)
			cites2 := upstream.CitationCollector{}
			readErr2 := stream2.Reader.Read(r.Context(), func(ev upstream.Event) error {
				cites2.Observe(ev, s.logf)
				defer wireDumpEvent(rtag, ev, 0, len(parser2.Calls()))
				switch ev.Type {
				case upstream.EventReasoningDelta:
					if text := ev.Reasoning(); text != "" && !write(openai.Delta{ReasoningContent: text}, nil) {
						return errClientGone
					}
				case upstream.EventTextStart, upstream.EventTextDelta:
					released := parser2.Feed([]byte(ev.ContentText()))
					if len(released) > 0 && !write(openai.Delta{Content: string(released)}, nil) {
						return errClientGone
					}
					emitted = emitToolCalls(w, flusher, parser2.Calls(), emitted, write)
				}
				return nil
			})
			if readErr2 == nil {
				cites = cites2
				wireDumpRound(rtag, string(parser2.Flush()), parser2.Calls())
				s.finishStream(w, flusher, write, parser2, cites, emitted)
				return
			}
			// Retry also failed at transport level: fall through to the
			// original stream's tail handling below (nothing was written).
			s.upErrors.Add(1)
			s.logf("upstream: retry stream failed after headers: %v", readErr2)
			readErr = readErr2
		} else {
			s.upErrors.Add(1)
			s.logf("upstream: retry open failed: %v", err2)
			readErr = err2
		}
	}
	if readErr != nil {
		s.streamFailure(w, r, flusher, readErr)
		return
	}
	wireDumpRound(rtag, string(parser.Flush()), parser.Calls())
	s.finishStream(w, flusher, write, parser, cites, emitted)
}

// streamNative consumes rounds privately until the model stops calling the
// gateway's web_search tool, then replays the final round as frames. Nothing
// from an internal round is ever written, so the first client-visible frame
// is always part of the answer the client asked for.
func (s *Server) streamNative(w http.ResponseWriter, r *http.Request, ureq *upstream.ChatRequest, model string, plan *toolPlan) {
	rtag := wireTag()
	res, calls, err := s.resolveNative(r.Context(), ureq, plan)
	wireDumpRound(rtag, res.content, calls)
	if err != nil {
		if nativeAbort(r, err) {
			return
		}
		s.fail(w, s.upstreamError(err))
		return
	}
	// The measured empty-answer failure mode can land on the final round too.
	// Nothing has been written yet, so the same one-shot retry applies.
	if res.emptyToolAnswer() && len(calls) == 0 {
		s.logf("tools: empty answer round (no text/call), retrying once")
		if retry, retryCalls, err2 := s.resolveNative(r.Context(), ureq, plan); err2 == nil {
			res, calls = retry, retryCalls
		} else if nativeAbort(r, err2) {
			return
		} else {
			s.logf("tools: native retry failed: %v", err2)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	s.streams.Add(1)

	write := s.chunkWriter(w, flusher, upstream.RandomID("chatcmpl"), s.now().Unix(), model, rtag)
	if !write(openai.Delta{Role: "assistant"}, nil) {
		return
	}
	s.replayRound(w, flusher, write, res, calls)
}

// resolveNative runs the internal web_search loop: read a round, serve every
// native call, append its result, and re-ask. It returns the first round with
// no native call plus any client calls accumulated from internal rounds (they
// are emitted with the final answer so no client tool call is lost).
func (s *Server) resolveNative(ctx context.Context, ureq *upstream.ChatRequest, plan *toolPlan) (*round, []tools.Call, error) {
	manifest := plan.manifest
	hist := plan.hist
	effort := ureq.ReasoningEffort
	budget := 0
	var pending []tools.Call
	// One round per allowed search, plus the round that reports the budget
	// exhaustion, plus a final round the model answers in. A model that keeps
	// calling past that is resolved with the calls stripped rather than looped.
	limit := nativeSearchBudget + 2
	var last *round
	for round := 0; round < limit; round++ {
		res, err := s.readRound(ctx, upstream.NewToolChatRequestHistory(hist, effort, manifest), manifest)
		if err != nil {
			return nil, nil, err
		}
		last = res
		native, client := nativeRound(res.calls)
		if len(native) == 0 {
			return res, append(pending, client...), nil
		}
		step, used, err := s.serveNativeCalls(ctx, native, res.content, budget)
		if err != nil {
			return nil, nil, err
		}
		budget = used
		hist = append(hist, step...)
		pending = append(pending, client...)
	}
	// The model ignored the budget and the round cap: answer with its last
	// round, with the unresolved native calls stripped. A refusal-by-loop is
	// not a client-visible error; the stream still closes normally.
	s.logf("tools: native search round cap reached; answering with the last round")
	return last, pending, nil
}

// replayRound writes an already-consumed round as the client-visible answer.
// The tool_calls finish reason is decided by the calls that remain after the
// native ones were served.
func (s *Server) replayRound(w http.ResponseWriter, flusher http.Flusher, write func(openai.Delta, *string) bool, res *round, calls []tools.Call) {
	if res.reasoning != "" && !write(openai.Delta{ReasoningContent: res.reasoning}, nil) {
		return
	}
	if res.content != "" && !write(openai.Delta{Content: res.content}, nil) {
		return
	}
	emitToolCalls(w, flusher, calls, 0, write)
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	if !write(openai.Delta{Annotations: res.cites.Citations()}, &finish) {
		return
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// finishStream flushes a parser's tail, emits its calls and closes the SSE
// response.
func (s *Server) finishStream(w http.ResponseWriter, flusher http.Flusher, write func(openai.Delta, *string) bool, parser *tools.Parser, cites upstream.CitationCollector, emitted int) {
	if tail := parser.Flush(); len(tail) > 0 {
		if !write(openai.Delta{Content: string(tail)}, nil) {
			return
		}
	}
	emitToolCalls(w, flusher, parser.Calls(), emitted, write)
	finish := "stop"
	if len(parser.Calls()) > 0 {
		finish = "tool_calls"
	}
	if !write(openai.Delta{Annotations: cites.Citations()}, &finish) {
		return
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// streamFailure writes the mid-stream error envelope unless the client is
// already gone.
func (s *Server) streamFailure(w http.ResponseWriter, r *http.Request, flusher http.Flusher, readErr error) {
	if errors.Is(readErr, errClientGone) {
		return
	}
	if errors.Is(readErr, context.Canceled) && r.Context().Err() != nil {
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
}

// chunkWriter builds the frame writer for one streamed response.
func (s *Server) chunkWriter(w http.ResponseWriter, flusher http.Flusher, id string, created int64, model string, tag string) func(openai.Delta, *string) bool {
	return func(delta openai.Delta, finish *string) bool {
		chunk := openai.Chunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
		if err := writeEvent(w, chunk); err != nil {
			return false
		}
		wireDumpChunk(tag, chunk)
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
}

// completeTools is the non-streaming path for a plain manifest.
func (s *Server) completeTools(w http.ResponseWriter, r *http.Request, ureq *upstream.ChatRequest, model string, manifest *tools.Manifest, plan *toolPlan) {
	var res *round
	var calls []tools.Call
	var err error
	if plan.native {
		res, calls, err = s.resolveNative(r.Context(), ureq, plan)
	} else {
		res, err = s.readRound(r.Context(), ureq, manifest)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return
		}
		s.fail(w, s.upstreamError(err))
		return
	}
	if plan.native {
		s.writeToolCompletion(w, model, res.content, res.reasoning, calls, res.cites.Citations())
		return
	}
	s.writeToolCompletion(w, model, res.content, res.reasoning, res.calls, res.cites.Citations())
}

// writeToolCompletion writes the aggregated chat.completion for tool output.
func (s *Server) writeToolCompletion(w http.ResponseWriter, model, content, reasoning string, calls []tools.Call, cites []openai.Citation) {
	msg := openai.CompletionMessage{
		Role:             "assistant",
		Content:          content,
		ReasoningContent: reasoning,
		Annotations:      cites,
	}
	finish := "stop"
	if len(calls) > 0 {
		msg.ToolCalls = make([]openai.ToolCall, 0, len(calls))
		for _, c := range calls {
			msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
				ID:       tools.NewID(),
				Type:     "function",
				Function: openai.FunctionCall{Name: c.Name, Arguments: c.Arguments},
			})
		}
		finish = "tool_calls"
	}
	writeJSON(w, http.StatusOK, openai.Completion{
		ID:      upstream.RandomID("chatcmpl"),
		Object:  "chat.completion",
		Created: s.now().Unix(),
		Model:   model,
		Choices: []openai.CompletionChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finish,
		}},
	})
}
