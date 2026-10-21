package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// Speech is the upstream audio abstraction the handlers depend on. It is
// separate from Chatter so a deployment can wire audio independently.
type Speech interface {
	Speech(ctx context.Context, req upstream.SpeechRequest, timeout time.Duration) ([]byte, error)
	Transcribe(ctx context.Context, up upstream.AudioUpload, timeout time.Duration) (string, error)
}

// defaultAudioMaxBytes caps a transcription upload. It matches OpenAI's own
// limit but is not the upstream's: the live upstream accepted a 2.5 MB body
// (SPEC §2.5.1, probe 6), so this number is the gateway's memory/latency
// bound, not a translation of an upstream rule. It is far above the 6 MiB
// JSON cap because recordings legitimately run to tens of megabytes.
const defaultAudioMaxBytes = 25 << 20

// defaultAudioTimeout bounds the pre-header phase of one audio call. The live
// TTS answered in about a second; the window is generous for long inputs.
const defaultAudioTimeout = 120 * time.Second

// SetSpeech attaches the audio backend. Without one both audio routes answer
// 503 rather than panicking.
func (s *Server) SetSpeech(sp Speech) { s.audio = sp }

// audioTimeout returns the configured audio timeout or its default.
func (s *Server) audioTimeout() time.Duration {
	if s.cfg.AudioTimeout > 0 {
		return s.cfg.AudioTimeout
	}
	return defaultAudioTimeout
}

// audioMaxBytes returns the configured upload cap or its default.
func (s *Server) audioMaxBytes() int64 {
	if s.cfg.AudioMaxBytes > 0 {
		return s.cfg.AudioMaxBytes
	}
	return defaultAudioMaxBytes
}

// voiceAliases maps OpenAI's standard voice names onto the four upstream
// voices (SPEC §2.5.1). The upstream does not validate voice names: an unknown
// one answers 200 with a short nonsense clip. Under the pass-through rule that
// answer is the upstream's to give, so a name outside this table is forwarded
// unchanged rather than rejected — the table is a translation for OpenAI
// clients, not a whitelist.
var voiceAliases = map[string]string{
	"alloy":   "sarah",
	"echo":    "chris",
	"fable":   "charlie",
	"onyx":    "chris",
	"nova":    "jessica",
	"shimmer": "jessica",
}

// resolveVoice maps an OpenAI voice name onto an upstream voice and lowercases
// every other name, leaving it for the upstream to interpret. An empty name
// means "the upstream default" and returns "".
func resolveVoice(name string) string {
	v := strings.ToLower(strings.TrimSpace(name))
	if mapped, ok := voiceAliases[v]; ok {
		return mapped
	}
	return v
}

// speechRequest is the POST /v1/audio/speech body. Only the structurally
// required field is typed; model, response_format, speed and every other key
// are dropped (SPEC §1.1).
type speechRequest struct {
	Input string `json:"input"`
	Voice string `json:"voice"`
}

// transcriptionResponse is OpenAI's `json` transcription shape.
type transcriptionResponse struct {
	Object string `json:"object"`
	Text   string `json:"text"`
}

// speech bridges OpenAI's speech request onto /api/tts. The upstream body is
// read to completion before headers are committed, so an upstream failure
// still produces a JSON error envelope instead of a truncated audio stream.
func (s *Server) speech(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	s.requests.Add(1)
	s.tts.Add(1)

	body, re := s.readJSONBody(w, r)
	if re != nil {
		s.fail(w, re)
		return
	}
	req, re := decodeSpeech(body)
	if re != nil {
		s.fail(w, re)
		return
	}
	if s.audio == nil {
		s.fail(w, errNoAudioBackend)
		return
	}

	audio, err := s.audio.Speech(r.Context(), upstream.SpeechRequest{Text: req.Input, Voice: resolveVoice(req.Voice)}, s.audioTimeout())
	if err != nil {
		if clientGone(r, err) {
			return
		}
		s.fail(w, s.upstreamError(err))
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(audio)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}

// transcription bridges OpenAI's multipart upload onto /api/stt. The file is
// forwarded byte-for-byte with a sniffed part type; nothing is transcoded
// (SPEC §2.5.2).
func (s *Server) transcription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	s.requests.Add(1)
	s.stt.Add(1)

	limit := s.audioMaxBytes()
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	up, re := readAudioUpload(w, r, limit)
	if re != nil {
		s.fail(w, re)
		return
	}
	if s.audio == nil {
		s.fail(w, errNoAudioBackend)
		return
	}

	text, err := s.audio.Transcribe(r.Context(), up, s.audioTimeout())
	if err != nil {
		if clientGone(r, err) {
			return
		}
		s.fail(w, s.upstreamError(err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, transcriptionResponse{Object: "transcription", Text: text})
}

// errNoAudioBackend is what both routes answer when no Speech backend is
// attached; it is a configuration fault, not an upstream one.
var errNoAudioBackend = openai.New(http.StatusServiceUnavailable, "server_error", openai.CodeUpstreamUnavailable, "",
	"audio backend is not configured")

// clientGone reports whether err is just the caller hanging up: writing an
// envelope to a closed socket would only log a spurious upstream error.
func clientGone(r *http.Request, err error) bool {
	return errors.Is(err, context.Canceled) && r.Context().Err() != nil
}

// decodeSpeech accepts only what the upstream call cannot express: malformed
// JSON, trailing data, a non-object body, and an empty or non-string input.
// Everything else is dropped (SPEC §7.2).
func decodeSpeech(body []byte) (*speechRequest, *openai.RequestError) {
	if _, re := decodeJSONObject(body); re != nil {
		return nil, re
	}

	var req speechRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// A non-string input (number, array, object) lands here.
		return nil, &openai.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: openai.CodeInvalidRequest, Param: "input", Message: `field "input" must be a string`}
	}
	if strings.TrimSpace(req.Input) == "" {
		return nil, &openai.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: openai.CodeInvalidRequest, Param: "input", Message: `field "input" must be a non-empty string`}
	}
	return &req, nil
}

// readAudioUpload reads the multipart body's `file` part. Missing parts,
// unreadable bodies, and empty files are structural 400s; an oversized body
// is a 413. The bytes are returned unchanged.
func readAudioUpload(w http.ResponseWriter, r *http.Request, limit int64) (upstream.AudioUpload, *openai.RequestError) {
	badRequest := func(msg string) (upstream.AudioUpload, *openai.RequestError) {
		return upstream.AudioUpload{}, &openai.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: openai.CodeInvalidRequest, Param: "file", Message: msg}
	}

	reader, err := r.MultipartReader()
	if err != nil {
		return badRequest("request must be multipart/form-data with a \"file\" part")
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return badRequest(`missing required part "file"`)
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return upstream.AudioUpload{}, openai.New(http.StatusRequestEntityTooLarge, "invalid_request_error",
					openai.CodeRequestTooLarge, "file", fmt.Sprintf("upload exceeds %d bytes", limit))
			}
			return badRequest(fmt.Sprintf("read multipart body: %v", err))
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		data, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return upstream.AudioUpload{}, openai.New(http.StatusRequestEntityTooLarge, "invalid_request_error",
					openai.CodeRequestTooLarge, "file", fmt.Sprintf("upload exceeds %d bytes", limit))
			}
			return badRequest(fmt.Sprintf("read file part: %v", err))
		}
		if len(data) == 0 {
			return badRequest(`part "file" is empty`)
		}
		name := part.FileName()
		if name == "" {
			name = "audio.mp3"
		}
		return upstream.AudioUpload{Filename: name, Data: data}, nil
	}
}
