package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"inceptionlabs2api/internal/upstream"
)

// Decoding helpers pinned to the exact OpenAI wire shape (SPEC §2.4).

type decodedCitation struct {
	Type       string `json:"type"`
	Index      int    `json:"index"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
}

type decodedChunkWithAnnotations struct {
	Choices []struct {
		Delta struct {
			Content     string            `json:"content"`
			Annotations []decodedCitation `json:"annotations"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// annotationsByFinish returns the annotations carried by the finish frame and
// asserts no other frame carries any: the SPEC pins annotations to the finish
// chunk.
func annotationsByFinish(t *testing.T, body string) []decodedCitation {
	t.Helper()
	var found []decodedCitation
	var finishSeen bool
	for _, f := range sseFrames(t, body) {
		if f == "[DONE]" {
			continue
		}
		var c decodedChunkWithAnnotations
		if err := json.Unmarshal([]byte(f), &c); err != nil {
			t.Fatalf("chunk %q: %v", f, err)
		}
		if len(c.Choices) == 0 {
			continue
		}
		ch := c.Choices[0]
		if len(ch.Delta.Annotations) > 0 {
			if ch.FinishReason == nil || *ch.FinishReason != "stop" {
				t.Fatalf("annotations must ride the finish frame, got finish_reason=%v", ch.FinishReason)
			}
			found = append(found, ch.Delta.Annotations...)
			finishSeen = true
		}
	}
	if !finishSeen && found != nil {
		t.Fatal("finish frame missing")
	}
	return found
}

const citedStream = `data: {"type":"reasoning-start","id":"reasoning-0"}
data: {"type":"reasoning-end","id":"reasoning-0"}
data: {"type":"source-url","sourceId":"__searching__","url":"","title":"__searching__"}
data: {"type":"source-url","sourceId":"https://a.example/x","url":"https://a.example/x","title":"A source"}
data: {"type":"source-url","sourceId":"https://b.example/y","url":"https://b.example/y","title":" \n B  source "}
data: {"type":"source-url","sourceId":"bad","url":"javascript:alert(1)","title":"bad"}
data: {"type":"reasoning-start","id":"reasoning-1"}
data: {"type":"reasoning-end","id":"reasoning-1"}
data: {"type":"text-start","id":"text-1"}
data: {"type":"text-delta","id":"text-1","delta":"Answer referencing [1] and [2]."}
data: {"type":"text-end","id":"text-1"}
data: [DONE]

`

func TestStreamCitationsOnFinishFrame(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(citedStream), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	cites := annotationsByFinish(t, body)
	if len(cites) != 2 {
		t.Fatalf("annotations = %+v, want 2", cites)
	}
	want := []decodedCitation{
		{Type: "url_citation", Index: 1, URL: "https://a.example/x", Title: "A source", StartIndex: 0, EndIndex: 0},
		{Type: "url_citation", Index: 2, URL: "https://b.example/y", Title: "B source", StartIndex: 0, EndIndex: 0},
	}
	for i := range want {
		if cites[i] != want[i] {
			t.Fatalf("annotation %d = %+v, want %+v", i, cites[i], want[i])
		}
	}
	if !strings.Contains(body, "Answer referencing") {
		t.Fatal("answer text lost")
	}
	for _, leak := range []string{"__searching__", "javascript:"} {
		if strings.Contains(body, leak) {
			t.Fatalf("stream leaked %q", leak)
		}
	}
	// The finish frame still terminates the stream and content order holds.
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatal("stream must end with [DONE]")
	}
}

// A response without sources keeps the exact pre-existing finish frame.
func TestStreamWithoutSourcesKeepsEmptyFinishDelta(t *testing.T) {
	t.Parallel()
	body := `data: {"type":"text-delta","id":"text-0","delta":"plain"}
data: [DONE]

`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(body), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	out := rec.Body.String()
	if strings.Contains(out, "annotations") {
		t.Fatalf("no sources must mean no annotations field: %s", out)
	}
	var sawFinish bool
	for _, f := range sseFrames(t, out) {
		if f == "[DONE]" {
			continue
		}
		c := decodeChunk(t, f)
		if c.Choices[0].FinishReason != nil {
			sawFinish = true
			if c.Choices[0].Delta.Content != "" || c.Choices[0].Delta.ReasoningContent != "" || len(c.Choices[0].Delta.Annotations) != 0 {
				t.Fatalf("finish delta = %+v, want empty", c.Choices[0].Delta)
			}
		}
	}
	if !sawFinish {
		t.Fatal("no finish frame")
	}
}

// A __search_error__ mid-answer must not abort the stream, and must not
// become an annotation.
func TestStreamSearchErrorSentinelIsNonFatal(t *testing.T) {
	t.Parallel()
	body := `data: {"type":"text-start","id":"text-0"}
data: {"type":"text-delta","id":"text-0","delta":"before "}
data: {"type":"source-url","sourceId":"__search_error__","url":"","title":"__search_error__"}
data: {"type":"text-delta","id":"text-0","delta":"after"}
data: {"type":"text-end","id":"text-0"}
data: [DONE]

`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(body), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	out := rec.Body.String()
	if strings.Contains(out, "upstream_stream_error") {
		t.Fatalf("search failure aborted the chat stream: %s", out)
	}
	if strings.Contains(out, "__search_error__") || strings.Contains(out, "annotations") {
		t.Fatalf("sentinel leaked into the response: %s", out)
	}
	var content strings.Builder
	for _, f := range sseFrames(t, out) {
		if f == "[DONE]" {
			continue
		}
		content.WriteString(decodeChunk(t, f).Choices[0].Delta.Content)
	}
	if content.String() != "before after" {
		t.Fatalf("content = %q, want the full answer", content.String())
	}
}

// Sources arriving after text-end but before a later text block are still
// collected (the live capture orders text-0 → sources → text-1).
func TestStreamSourcesAfterTextEnd(t *testing.T) {
	t.Parallel()
	body := `data: {"type":"text-start","id":"text-0"}
data: {"type":"text-delta","id":"text-0","delta":"first "}
data: {"type":"text-end","id":"text-0"}
data: {"type":"source-url","sourceId":"u1","url":"https://one.example","title":"One"}
data: {"type":"text-start","id":"text-1"}
data: {"type":"text-delta","id":"text-1","delta":"second"}
data: {"type":"text-end","id":"text-1"}
data: [DONE]

`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(body), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	cites := annotationsByFinish(t, rec.Body.String())
	if len(cites) != 1 || cites[0].URL != "https://one.example" {
		t.Fatalf("annotations = %+v, want the late source", cites)
	}
}

func TestNonStreamCitations(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(citedStream), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content     string            `json:"content"`
				Annotations []decodedCitation `json:"annotations"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg := body.Choices[0].Message
	if msg.Content != "Answer referencing [1] and [2]." {
		t.Fatalf("content = %q", msg.Content)
	}
	if len(msg.Annotations) != 2 {
		t.Fatalf("annotations = %+v, want 2", msg.Annotations)
	}
	if msg.Annotations[0].Index != 1 || msg.Annotations[1].Index != 2 {
		t.Fatalf("indices = %d,%d", msg.Annotations[0].Index, msg.Annotations[1].Index)
	}
	if msg.Annotations[0].Type != "url_citation" || msg.Annotations[0].StartIndex != 0 || msg.Annotations[0].EndIndex != 0 {
		t.Fatalf("annotation shape = %+v", msg.Annotations[0])
	}
	if strings.Contains(rec.Body.String(), "__searching__") || strings.Contains(rec.Body.String(), "javascript:") {
		t.Fatalf("sentinel or bad url leaked: %s", rec.Body.String())
	}
}

func TestNonStreamOmitsAnnotationsWithoutSources(t *testing.T) {
	t.Parallel()
	body := `data: {"type":"text-delta","id":"text-0","delta":"plain"}
data: [DONE]

`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(body), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}`)
	if strings.Contains(rec.Body.String(), "annotations") {
		t.Fatalf("annotations must be omitted with no sources: %s", rec.Body.String())
	}
}
