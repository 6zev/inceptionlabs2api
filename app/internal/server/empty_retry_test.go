package server

import (
	"net/http"
	"testing"
)

// The empty-completion retry and the message cap are the two behaviours taken
// from upstream PR #1 (bacteriaw/inceptionlabs2api#1), reimplemented on top of
// this tree's own fold and recovery machinery.

func TestEmptyCompletionRetriesThenSucceeds(t *testing.T) {
	mock := &mockChatter{rounds: []string{sseEmpty(), sseText("hello")}}
	s := newTestServer(t, mock)
	s.cfg.EmptyRetries = 2

	w := postChat(t, s, noToolsRequest("hi", false))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (empty then text)", got)
	}
	if c := decodeCompletion(t, w.Body.Bytes()); c.Choices[0].Message.Content != "hello" {
		t.Fatalf("content = %q, want hello", c.Choices[0].Message.Content)
	}
}

func TestEmptyCompletionExhaustsRetriesAndReturns502(t *testing.T) {
	mock := &mockChatter{rounds: []string{sseEmpty(), sseEmpty(), sseEmpty()}}
	s := newTestServer(t, mock)
	s.cfg.EmptyRetries = 2

	w := postChat(t, s, noToolsRequest("hi", false))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body %s, want 502", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 3 {
		t.Fatalf("upstream calls = %d, want 3 (1 + EmptyRetries)", got)
	}
}

func TestEmptyCompletionNoRetriesKeepsSingleAttempt(t *testing.T) {
	mock := &mockChatter{rounds: []string{sseEmpty()}}
	s := newTestServer(t, mock)
	s.cfg.EmptyRetries = 0

	w := postChat(t, s, noToolsRequest("hi", false))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if got := mock.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", got)
	}
}

func TestEmptyCompletionStreamReturns502(t *testing.T) {
	mock := &mockChatter{rounds: []string{sseEmpty()}}
	s := newTestServer(t, mock)

	w := postChat(t, s, noToolsRequest("hi", true))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body %s, want 502 (no 200 with an empty stream)", w.Code, w.Body.String())
	}
}

func TestEmptyAttemptsFloor(t *testing.T) {
	s := newTestServer(t, &mockChatter{})
	s.cfg.EmptyRetries = -5
	if got := s.emptyAttempts(); got != 1 {
		t.Fatalf("emptyAttempts() = %d, want 1", got)
	}
}

func TestTrimAppliedBeforeUpstream(t *testing.T) {
	mock := &mockChatter{rounds: []string{sseText("ok")}}
	s := newTestServer(t, mock)
	s.cfg.MaxMessages = 6

	const sent = 30
	w := postChat(t, s, foldChatBody(foldMessages(sent), false))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	req := mock.request(0)
	if req == nil {
		t.Fatal("no upstream request recorded")
	}
	if len(req.Messages) > s.cfg.MaxMessages {
		t.Fatalf("upstream saw %d messages, want <= %d", len(req.Messages), s.cfg.MaxMessages)
	}
}

// The empty-retry budget must hold on the TOOL path too, not just the plain
// one: EmptyRetries=1 means the original round plus exactly one retry.
func TestEmptyToolRoundRespectsRetryBudget(t *testing.T) {
	mock := &mockChatter{rounds: []string{sseEmpty(), sseEmpty(), sseEmpty(), sseEmpty()}}
	s := newTestServer(t, mock)
	s.cfg.EmptyRetries = 1

	w := postChat(t, s, weatherRequest(false))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body %s, want 502", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (original + EmptyRetries=1)", got)
	}
}

func TestEmptyToolRoundNoRetriesSingleAttempt(t *testing.T) {
	mock := &mockChatter{rounds: []string{sseEmpty(), sseEmpty()}}
	s := newTestServer(t, mock)
	s.cfg.EmptyRetries = 0

	w := postChat(t, s, weatherRequest(false))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if got := mock.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", got)
	}
}

// The cap must cover the TOOL path too: a long agent loop is the shape most
// likely to exceed the upstream's 100-message rejection.
func TestTrimAppliesToToolPath(t *testing.T) {
	mock := &mockChatter{rounds: []string{sseText("ok")}}
	s := newTestServer(t, mock)
	s.cfg.MaxMessages = 6

	msgs := foldMessages(30)
	w := postChat(t, s, foldChatBody(msgs, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	req := mock.request(0)
	if req == nil {
		t.Fatal("no upstream request recorded")
	}
	// The manifest preamble occupies one upstream slot.
	if len(req.Messages) > s.cfg.MaxMessages+1 {
		t.Fatalf("upstream saw %d messages, want <= %d (+1 preamble)", len(req.Messages), s.cfg.MaxMessages)
	}
}
