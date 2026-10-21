package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"inceptionlabs2api/internal/pool"
)

// audioCallTimeout is the per-call bound the audio tests pass. It must cover
// the first clearance acquisition (a real PoW solve, ~1 s under -race) and is
// not the property under test; the stall tests pass their own short bound.
const audioCallTimeout = 30 * time.Second

// addAudioRoutes wires /api/tts and /api/stt onto the shared TLS mock.
func (m *mockUpstream) addAudioRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/tts", m.handleTTS)
	mux.HandleFunc("/api/stt", m.handleSTT)
}

func (m *mockUpstream) handleTTS(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.ttsTokens = append(m.ttsTokens, r.Header.Get("x-session-token"))
	body, _ := io.ReadAll(r.Body)
	m.ttsBodies = append(m.ttsBodies, string(body))
	h := m.ttsHandler
	m.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Write(testMP3)
}

func (m *mockUpstream) handleSTT(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.sttTokens = append(m.sttTokens, r.Header.Get("x-session-token"))
	m.sttContentTypes = append(m.sttContentTypes, r.Header.Get("Content-Type"))
	body, _ := io.ReadAll(r.Body)
	m.sttBodies = append(m.sttBodies, body)
	h := m.sttHandler
	m.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"text":"the quick brown fox"}`)
}

// testMP3 is a minimal but structurally MPEG-ish body: an ID3v2 header with
// an empty tag followed by one MPEG-1 Layer III frame header. It is only
// passed through, never decoded.
var testMP3 = append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), 0xFF, 0xFB, 0x90, 0xC4, 0x00, 0x00, 0x00, 0x00)

func TestSpeechSendsTextAndVoice(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	c, _ := newTestClient(t, m, 0)

	got, err := c.Speech(context.Background(), SpeechRequest{Text: "hello", Voice: "sarah"}, audioCallTimeout)
	if err != nil {
		t.Fatalf("Speech: %v", err)
	}
	if !bytes.Equal(got, testMP3) {
		t.Fatalf("body = %q, want the upstream bytes unchanged", got)
	}
	bodies := m.ttsRequests()
	if len(bodies) != 1 {
		t.Fatalf("tts calls = %d", len(bodies))
	}
	var sent map[string]string
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatalf("request body %q: %v", bodies[0], err)
	}
	if sent["text"] != "hello" || sent["voice"] != "sarah" {
		t.Fatalf("upstream body = %v", sent)
	}
	if _, ok := sent["model"]; ok {
		t.Fatalf("model must not be forwarded: %v", sent)
	}
}

func TestSpeechOmitsEmptyVoice(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	c, _ := newTestClient(t, m, 0)
	if _, err := c.Speech(context.Background(), SpeechRequest{Text: "hello"}, audioCallTimeout); err != nil {
		t.Fatalf("Speech: %v", err)
	}
	bodies := m.ttsRequests()
	var sent map[string]any
	json.Unmarshal([]byte(bodies[0]), &sent)
	if _, ok := sent["voice"]; ok {
		t.Fatalf("voice field present for an empty voice: %v", sent)
	}
}

func TestSpeechStatusError(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.ttsHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"bad text"}`)
	}
	c, _ := newTestClient(t, m, 0)
	_, err := c.Speech(context.Background(), SpeechRequest{Text: "x"}, audioCallTimeout)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want StatusError 400", err)
	}
	if se.Message() != "bad text" {
		t.Fatalf("message = %q", se.Message())
	}
}

func TestSpeechUnavailableWithoutPool(t *testing.T) {
	t.Parallel()
	c, _ := NewClient(Config{BaseURL: "https://127.0.0.1:1", Logf: func(string, ...any) {}})
	_, err := c.Speech(context.Background(), SpeechRequest{Text: "x"}, audioCallTimeout)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// The audio suite has no streaming watchdog, so the whole call (headers and
// body) must be bounded: an upstream that sends 200 and then stalls the body
// would otherwise hold the handler open until the client gives up. This is
// the audio analogue of the chat idle window.
func TestSpeechStalledBodyIsBounded(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	m.ttsHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	c, p := newTestClient(t, m, 1)
	// Warm the pool and clearance first: the audio timeout bounds the call,
	// and the test measures the body phase, not the one-off solve.
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatalf("warm pool: %v", err)
	}
	start := time.Now()
	_, err := c.Speech(context.Background(), SpeechRequest{Text: "x"}, 300*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout for a stalled body", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Speech waited %s on a stalled body", elapsed)
	}
}

func TestTranscribeStalledBodyIsBounded(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	m.sttHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	c, p := newTestClient(t, m, 1)
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatalf("warm pool: %v", err)
	}
	start := time.Now()
	_, err := c.Transcribe(context.Background(), AudioUpload{Filename: "a.mp3", Data: testMP3}, 300*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout for a stalled body", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Transcribe waited %s on a stalled body", elapsed)
	}
}

// When the whole-call deadline fires, the transport cancels the request; the
// upstream then closes the body cleanly (END_STREAM), so the body read can
// return io.EOF with zero bytes *before* it finds the cancellation. That must
// still be classified as the idle timeout it is, not as an empty body or an
// unparseable transcript. (Observed as a flake in the stalled-body tests: the
// error said "parse transcript: unexpected end of JSON input (body \"\")".)
func TestAudioDeadlineCutIsIdleTimeoutEvenOnCleanEOF(t *testing.T) {
	t.Parallel()
	// A stub transport whose 200 response body is already complete and empty.
	// With an expired parent context the call is cut by the deadline, yet the
	// body read returns a clean EOF: the classification must come from the
	// deadline, not from the bytes. Real-world reproduction: after the
	// whole-call timeout cancels the request, the upstream closes the body
	// cleanly and the read finds END_STREAM before it finds the cancellation.
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	c, p := newStubClient(t, rt)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := c.Speech(expired, SpeechRequest{Text: "x"}, time.Minute); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("Speech err = %v, want ErrIdleTimeout", err)
	}
	if _, err := c.Transcribe(expired, AudioUpload{Filename: "a.mp3", Data: testMP3}, time.Minute); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("Transcribe err = %v, want ErrIdleTimeout", err)
	}
	_ = p
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newStubClient builds a Client whose transport is rt over a pre-filled pool.
func newStubClient(t *testing.T, rt http.RoundTripper) (*Client, *pool.Pool) {
	t.Helper()
	c, _ := NewClient(Config{BaseURL: "https://stub.invalid", Logf: func(string, ...any) {}})
	c.hc.Transport = rt
	var issued int
	p := pool.New(pool.Config{Min: 1, Max: 8, TTL: time.Minute, Interval: time.Hour}, func(context.Context) (string, error) {
		issued++
		return "1792553057." + strings.Repeat("0", 31) + "a." + strings.Repeat("f", 64), nil
	})
	t.Cleanup(p.Close)
	c.pool = p
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatalf("warm pool: %v", err)
	}
	return c, p
}

func TestSpeechRejectsEmptyBody(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.ttsHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
	}
	c, _ := newTestClient(t, m, 0)
	if _, err := c.Speech(context.Background(), SpeechRequest{Text: "x"}, audioCallTimeout); err == nil {
		t.Fatal("a 200 with no bytes was accepted as audio")
	}
}

func TestTranscribeForwardsMultipartAndSniffsMPEG(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	c, _ := newTestClient(t, m, 0)

	text, err := c.Transcribe(context.Background(), AudioUpload{Filename: "speech.mp3", Data: testMP3}, audioCallTimeout)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "the quick brown fox" {
		t.Fatalf("text = %q", text)
	}
	calls := m.sttRequests()
	if len(calls) != 1 {
		t.Fatalf("stt calls = %d", len(calls))
	}
	mt, params, err := mime.ParseMediaType(calls[0].contentType)
	if err != nil || !strings.HasPrefix(mt, "multipart/form-data") {
		t.Fatalf("content-type = %q (%v)", calls[0].contentType, err)
	}
	part := firstPart(t, calls[0].body, params["boundary"])
	if part.field != "file" {
		t.Fatalf("field = %q", part.field)
	}
	if part.filename != "speech.mp3" {
		t.Fatalf("filename = %q", part.filename)
	}
	if part.contentType != "audio/mpeg" {
		t.Fatalf("part content-type = %q, want audio/mpeg for MPEG bytes", part.contentType)
	}
	if !bytes.Equal(part.data, testMP3) {
		t.Fatalf("uploaded bytes were rewritten")
	}
}

func TestTranscribeTypesNonMPEGAsOctetStream(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	c, _ := newTestClient(t, m, 0)
	wav := append([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), make([]byte, 16)...)
	if _, err := c.Transcribe(context.Background(), AudioUpload{Filename: "a.wav", Data: wav}, audioCallTimeout); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	calls := m.sttRequests()
	_, params, _ := mime.ParseMediaType(calls[0].contentType)
	part := firstPart(t, calls[0].body, params["boundary"])
	if part.contentType != "application/octet-stream" {
		t.Fatalf("part content-type = %q, want application/octet-stream for WAV", part.contentType)
	}
	if !bytes.Equal(part.data, wav) {
		t.Fatalf("WAV bytes were rewritten")
	}
}

func TestTranscribeEmptyTextIsNotAnError(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.sttHandler = func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"text":""}`) }
	c, _ := newTestClient(t, m, 0)
	text, err := c.Transcribe(context.Background(), AudioUpload{Filename: "a.wav", Data: []byte("RIFF....")}, audioCallTimeout)
	if err != nil {
		t.Fatalf("empty text became an error: %v", err)
	}
	if text != "" {
		t.Fatalf("text = %q", text)
	}
}

func TestTranscribeUpstreamInvalidFileType(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.sttHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"Invalid file type"}`)
	}
	c, _ := newTestClient(t, m, 0)
	_, err := c.Transcribe(context.Background(), AudioUpload{Filename: "a.bin", Data: []byte("nope")}, audioCallTimeout)
	var se *StatusError
	if !errors.As(err, &se) || se.Message() != "Invalid file type" {
		t.Fatalf("err = %v", err)
	}
}

func TestTranscribeRejectsUnparseableBody(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.sttHandler = func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>") }
	c, _ := newTestClient(t, m, 0)
	_, err := c.Transcribe(context.Background(), AudioUpload{Filename: "a.mp3", Data: testMP3}, audioCallTimeout)
	if err == nil {
		t.Fatal("html body parsed as a transcript")
	}
}

type multipartPart struct {
	field, filename, contentType string
	data                         []byte
}

func firstPart(t *testing.T, body []byte, boundary string) multipartPart {
	t.Helper()
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	p, err := mr.NextPart()
	if err != nil {
		t.Fatalf("read multipart: %v", err)
	}
	defer p.Close()
	data, _ := io.ReadAll(p)
	return multipartPart{field: p.FormName(), filename: p.FileName(), contentType: p.Header.Get("Content-Type"), data: data}
}

func TestLooksLikeMPEG(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"id3", []byte("ID3\x04\x00\x00"), true},
		{"frame sync", []byte{0xFF, 0xFB, 0x90, 0xC4}, true},
		{"frame sync after tag", append([]byte("ID3\x04\x00\x00\x00\x00\x00\x0a"), 0xFF, 0xFB, 0x90, 0xC4), true},
		{"wav", append([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), make([]byte, 16)...), false},
		{"webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x01, 0x00}, false},
		{"empty", nil, false},
		{"short", []byte{0xFF}, false},
		// 0xFF followed by a reserved layer/version combination is not a frame.
		{"bad frame", []byte{0xFF, 0xF1, 0x90, 0xC4}, false},
		{"frame not at offset 0", []byte{0x00, 0xFF, 0xFB, 0x90, 0xC4}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := LooksLikeMPEG(tc.in); got != tc.want {
				t.Fatalf("LooksLikeMPEG(%x) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
