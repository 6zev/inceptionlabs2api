package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// fakeSpeech scripts the upstream audio abstraction per call.
type fakeSpeech struct {
	mu      sync.Mutex
	reqs    []upstream.SpeechRequest
	speech  func(call int) ([]byte, error)
	uploads []upstream.AudioUpload
	stt     func(call int) (string, error)
}

func (f *fakeSpeech) Speech(ctx context.Context, req upstream.SpeechRequest, timeout time.Duration) ([]byte, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	call := len(f.reqs)
	fn := f.speech
	f.mu.Unlock()
	if fn == nil {
		return []byte("ID3\x04\x00\x00\x00\x00\x00\x00\xff\xfb\x90\xc4"), nil
	}
	return fn(call)
}

func (f *fakeSpeech) Transcribe(ctx context.Context, up upstream.AudioUpload, timeout time.Duration) (string, error) {
	f.mu.Lock()
	f.uploads = append(f.uploads, up)
	call := len(f.uploads)
	fn := f.stt
	f.mu.Unlock()
	if fn == nil {
		return "hello world", nil
	}
	return fn(call)
}

func (f *fakeSpeech) lastSpeech() (upstream.SpeechRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return upstream.SpeechRequest{}, false
	}
	return f.reqs[len(f.reqs)-1], true
}

func (f *fakeSpeech) lastUpload() (upstream.AudioUpload, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploads) == 0 {
		return upstream.AudioUpload{}, false
	}
	return f.uploads[len(f.uploads)-1], true
}

func newAudioServer(sp Speech) *Server {
	s := newTestServer(&fakeChatter{})
	s.SetSpeech(sp)
	return s
}

func postSpeech(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body)))
	return rec
}

func TestSpeechResponseIsMP3(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{}
	s := newAudioServer(sp)
	rec := postSpeech(t, s, `{"model":"tts-1","input":"Hello there.","voice":"alloy","response_format":"mp3","speed":1.0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("content-type = %q", ct)
	}
	if got := rec.Body.Bytes(); !bytes.HasPrefix(got, []byte("ID3")) {
		t.Fatalf("body is not the upstream audio: %q", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
	req, ok := sp.lastSpeech()
	if !ok {
		t.Fatal("upstream was not called")
	}
	if req.Text != "Hello there." {
		t.Fatalf("text = %q", req.Text)
	}
	// OpenAI's `alloy` maps onto an upstream voice from the whitelist.
	if req.Voice != "sarah" {
		t.Fatalf("voice = %q, want the alloy mapping", req.Voice)
	}
}

func TestSpeechResponseFormatIsAlwaysMP3(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{}
	s := newAudioServer(sp)
	for _, format := range []string{"wav", "opus", "aac", "flac", "pcm", ""} {
		body, _ := json.Marshal(map[string]any{"input": "hi", "response_format": format})
		rec := postSpeech(t, s, string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("format %q: status = %d: %s", format, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
			t.Fatalf("format %q: content-type = %q", format, ct)
		}
	}
}

func TestSpeechVoiceMapping(t *testing.T) {
	t.Parallel()
	// OpenAI's standard voice names all map onto the four upstream voices.
	want := map[string]string{
		"alloy":   "sarah",
		"echo":    "chris",
		"fable":   "charlie",
		"onyx":    "chris",
		"nova":    "jessica",
		"shimmer": "jessica",
		// Upstream names pass through unchanged.
		"sarah":   "sarah",
		"chris":   "chris",
		"charlie": "charlie",
		"jessica": "jessica",
		// Case is folded.
		"SARAH": "sarah",
		"Nova":  "jessica",
	}
	for in, out := range want {
		in, out := in, out
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			sp := &fakeSpeech{}
			s := newAudioServer(sp)
			body, _ := json.Marshal(map[string]any{"input": "hi", "voice": in})
			rec := postSpeech(t, s, string(body))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			req, _ := sp.lastSpeech()
			if req.Voice != out {
				t.Fatalf("voice %q -> %q, want %q", in, req.Voice, out)
			}
		})
	}
}

func TestSpeechUnknownVoiceIs400(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{}
	s := newAudioServer(sp)
	rec := postSpeech(t, s, `{"input":"hi","voice":"nonexistent-voice"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var env openai.ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Param != "voice" || env.Error.Code != openai.CodeInvalidRequest {
		t.Fatalf("envelope = %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "sarah") {
		t.Fatalf("message does not list the accepted voices: %q", env.Error.Message)
	}
	if _, ok := sp.lastSpeech(); ok {
		t.Fatal("an unknown voice reached the upstream")
	}
}

func TestSpeechEmptyVoiceUsesUpstreamDefault(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{}
	s := newAudioServer(sp)
	rec := postSpeech(t, s, `{"input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	req, _ := sp.lastSpeech()
	if req.Voice != "" {
		t.Fatalf("voice = %q, want empty (upstream default)", req.Voice)
	}
}

func TestSpeechStructuralRejections(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body string
		want int
		parm string
	}{
		"missing input":   {`{"voice":"sarah"}`, 400, "input"},
		"empty input":     {`{"input":""}`, 400, "input"},
		"whitespace":      {`{"input":"   \n"}`, 400, "input"},
		"non-string":      {`{"input":42}`, 400, "input"},
		"malformed json":  {`{"input":`, 400, ""},
		"not an object":   {`[]`, 400, ""},
		"trailing data":   {`{"input":"hi"} {}`, 400, ""},
		"input null":      {`{"input":null}`, 400, "input"},
		"unknown keys go": {`{"input":"hi","temperature":0.5,"n":3,"user":"u"}`, 200, ""},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sp := &fakeSpeech{}
			s := newAudioServer(sp)
			rec := postSpeech(t, s, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != 200 {
				var env openai.ErrorResponse
				json.Unmarshal(rec.Body.Bytes(), &env)
				if tc.parm != "" && env.Error.Param != tc.parm {
					t.Fatalf("param = %q, want %q", env.Error.Param, tc.parm)
				}
				if _, ok := sp.lastSpeech(); ok {
					t.Fatal("a rejected request reached the upstream")
				}
			}
		})
	}
}

func TestSpeechLongInputPassesThrough(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{}
	s := newAudioServer(sp)
	long := strings.Repeat("word ", 2000) // 10 000 chars, above the bundle's 5 000 slice
	body, _ := json.Marshal(map[string]any{"input": long})
	rec := postSpeech(t, s, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	req, _ := sp.lastSpeech()
	if req.Text != long {
		t.Fatalf("text was truncated to %d chars", len(req.Text))
	}
}

func TestSpeechUpstreamErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"status", &upstream.StatusError{Status: 500, Body: `{"error":"tts exploded"}`}, 502, openai.CodeUpstreamError},
		{"unavailable", upstream.ErrUnavailable, 503, openai.CodeUpstreamUnavailable},
		{"clearance", upstream.ErrClearance, 503, openai.CodeUpstreamClearance},
		{"idle", upstream.ErrIdleTimeout, 504, openai.CodeUpstreamIdle},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sp := &fakeSpeech{speech: func(int) ([]byte, error) { return nil, tc.err }}
			s := newAudioServer(sp)
			rec := postSpeech(t, s, `{"input":"hi"}`)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			var env openai.ErrorResponse
			json.Unmarshal(rec.Body.Bytes(), &env)
			if env.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", env.Error.Code, tc.code)
			}
			if tc.name == "status" && env.Error.Message != "tts exploded" {
				t.Fatalf("upstream message lost: %q", env.Error.Message)
			}
		})
	}
}

func TestSpeechWithoutBackendIs503(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	rec := postSpeech(t, s, `{"input":"hi"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSpeechMethodNotAllowed(t *testing.T) {
	t.Parallel()
	s := newAudioServer(&fakeSpeech{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audio/speech", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Allow") != "POST" {
		t.Fatalf("allow = %q", rec.Header().Get("Allow"))
	}
}

// multipartUpload builds a request body with one `file` part.
func multipartUpload(t *testing.T, field, filename, partType string, data []byte, extra map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h["Content-Disposition"] = []string{`form-data; name="` + field + `"; filename="` + filename + `"`}
	if partType != "" {
		h["Content-Type"] = []string{partType}
	}
	w, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	w.Write(data)
	for k, v := range extra {
		mw.WriteField(k, v)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func postTranscription(t *testing.T, s *Server, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestTranscriptionResponseShape(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{}
	s := newAudioServer(sp)
	body, ct := multipartUpload(t, "file", "speech.mp3", "audio/mpeg", []byte("ID3\x04\x00\x00"), map[string]string{"model": "whisper-1", "language": "en"})
	rec := postTranscription(t, s, body, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["object"] != "transcription" || got["text"] != "hello world" {
		t.Fatalf("body = %v", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
	up, ok := sp.lastUpload()
	if !ok {
		t.Fatal("upstream not called")
	}
	if up.Filename != "speech.mp3" || !bytes.Equal(up.Data, []byte("ID3\x04\x00\x00")) {
		t.Fatalf("upload = %+v", up)
	}
}

func TestTranscriptionKeepsEmptyText(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{stt: func(int) (string, error) { return "", nil }}
	s := newAudioServer(sp)
	body, ct := multipartUpload(t, "file", "a.wav", "audio/wav", []byte("RIFF....WAVE"), nil)
	rec := postTranscription(t, s, body, ct)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["text"] != "" {
		t.Fatalf("text = %v", got["text"])
	}
	if _, ok := got["text"]; !ok {
		t.Fatal("text field missing")
	}
}

func TestTranscriptionStructuralRejections(t *testing.T) {
	t.Parallel()
	t.Run("not multipart", func(t *testing.T) {
		t.Parallel()
		sp := &fakeSpeech{}
		rec := postTranscription(t, newAudioServer(sp), strings.NewReader(`{"file":"x"}`), "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
		if _, ok := sp.lastUpload(); ok {
			t.Fatal("reached upstream")
		}
	})
	t.Run("no file part", func(t *testing.T) {
		t.Parallel()
		sp := &fakeSpeech{}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("model", "whisper-1")
		mw.Close()
		rec := postTranscription(t, newAudioServer(sp), &buf, mw.FormDataContentType())
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("wrong field name", func(t *testing.T) {
		t.Parallel()
		sp := &fakeSpeech{}
		body, ct := multipartUpload(t, "audio", "a.mp3", "audio/mpeg", []byte("ID3"), nil)
		rec := postTranscription(t, newAudioServer(sp), body, ct)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		t.Parallel()
		sp := &fakeSpeech{}
		body, ct := multipartUpload(t, "file", "a.mp3", "audio/mpeg", nil, nil)
		rec := postTranscription(t, newAudioServer(sp), body, ct)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestTranscriptionOversizeIs413(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{}
	s := newAudioServer(sp)
	s.cfg.AudioMaxBytes = 1024
	big := bytes.Repeat([]byte("ID3"), 2048)
	body, ct := multipartUpload(t, "file", "big.mp3", "audio/mpeg", big, nil)
	rec := postTranscription(t, s, body, ct)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := sp.lastUpload(); ok {
		t.Fatal("an oversized upload reached the upstream")
	}
}

func TestTranscriptionUpstreamError(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{stt: func(int) (string, error) {
		return "", &upstream.StatusError{Status: 400, Body: `{"error":"Invalid file type"}`}
	}}
	s := newAudioServer(sp)
	body, ct := multipartUpload(t, "file", "a.bin", "application/octet-stream", []byte("nope"), nil)
	rec := postTranscription(t, s, body, ct)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var env openai.ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Message != "Invalid file type" {
		t.Fatalf("message = %q", env.Error.Message)
	}
}

func TestTranscriptionMethodNotAllowed(t *testing.T) {
	t.Parallel()
	s := newAudioServer(&fakeSpeech{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audio/transcriptions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHealthzAudioCounters(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{}
	s := newAudioServer(sp)
	postSpeech(t, s, `{"input":"hi"}`)
	postSpeech(t, s, `{"input":""}`) // rejected, but counted as a request
	body, ct := multipartUpload(t, "file", "a.mp3", "audio/mpeg", []byte("ID3"), nil)
	postTranscription(t, s, body, ct)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var got struct {
		Counters Counters `json:"counters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Counters.TTS != 2 || got.Counters.STT != 1 {
		t.Fatalf("counters = %+v", got.Counters)
	}
}

func TestHealthzAudioCountersPresentWhenZero(t *testing.T) {
	t.Parallel()
	s := newAudioServer(&fakeSpeech{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !strings.Contains(rec.Body.String(), `"tts":0`) || !strings.Contains(rec.Body.String(), `"stt":0`) {
		t.Fatalf("healthz = %s", rec.Body.String())
	}
}

func TestSpeechErrorIsNotCountedAsUpstreamWhenStructural(t *testing.T) {
	t.Parallel()
	sp := &fakeSpeech{}
	s := newAudioServer(sp)
	postSpeech(t, s, `{"input":""}`)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var got struct {
		Counters Counters `json:"counters"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Counters.Upstream != 0 {
		t.Fatalf("structural 400 counted as an upstream error: %+v", got.Counters)
	}
}
