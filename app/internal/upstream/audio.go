package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// maxAudioBytes caps a TTS response body and an STT upload. TTS clips are
// bounded in practice by the client's 5 000-char text slice (10 s for 155
// chars), so 32 MiB is far above any real body and exists only to stop an
// upstream that never stops talking.
const maxAudioBytes = 32 << 20

// maxTranscriptBytes caps the STT JSON body. The upstream returns one short
// object.
const maxTranscriptBytes = 1 << 20

// SpeechRequest is the POST /api/tts body. The upstream has exactly these two
// fields; everything else in an OpenAI speech request is the gateway's job to
// drop (SPEC §2.5.1).
type SpeechRequest struct {
	Text  string `json:"text"`
	Voice string `json:"voice,omitempty"`
}

// AudioUpload is one transcription input. The bytes are forwarded unchanged;
// Filename and ContentType only shape the multipart part.
type AudioUpload struct {
	Filename    string
	Data        []byte
	ContentType string
}

// Speech posts req to /api/tts and returns the complete MP3 body. The bytes
// are never decoded or rewritten: the upstream is the encoder.
func (c *Client) Speech(ctx context.Context, req SpeechRequest, timeout time.Duration) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: marshal speech request: %w", err)
	}
	resp, err := c.postAuthed(ctx, "/api/tts", "tts", body, timeout, func(r *http.Request) {
		r.Header.Set("Accept", "audio/mpeg, */*")
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	audio, err := io.ReadAll(io.LimitReader(resp.Body, maxAudioBytes+1))
	if err != nil {
		return nil, fmt.Errorf("upstream: read speech body: %w", err)
	}
	if len(audio) > maxAudioBytes {
		return nil, fmt.Errorf("upstream: speech body exceeds %d bytes", maxAudioBytes)
	}
	if len(audio) == 0 {
		// A 200 with no bytes is not an audio file. Surface it as an upstream
		// fault rather than serving an empty body as if it were speech.
		return nil, fmt.Errorf("upstream: speech response had an empty body")
	}
	return audio, nil
}

// Transcribe posts upload to /api/stt and returns the upstream transcript
// verbatim. The uploaded bytes are never transcoded (SPEC §2.5.2); only the
// multipart part's content type is chosen, by sniffing.
func (c *Client) Transcribe(ctx context.Context, up AudioUpload, timeout time.Duration) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	partType := up.ContentType
	if partType == "" {
		if LooksLikeMPEG(up.Data) {
			partType = "audio/mpeg"
		} else {
			// The upstream only rejects a part with no audio/* type outright
			// ("Invalid file type"); application/octet-stream reproduces the
			// site's own behaviour for unknown bytes and lets the upstream
			// answer the format question (an empty transcript).
			partType = "application/octet-stream"
		}
	}
	name := up.Filename
	if name == "" {
		name = "audio.mp3"
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(name)))
	h.Set("Content-Type", partType)
	w, err := mw.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("upstream: build stt multipart: %w", err)
	}
	if _, err := w.Write(up.Data); err != nil {
		return "", fmt.Errorf("upstream: build stt multipart: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("upstream: build stt multipart: %w", err)
	}

	resp, err := c.postAuthed(ctx, "/api/stt", "stt", buf.Bytes(), timeout, func(r *http.Request) {
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.Header.Set("Accept", "application/json")
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTranscriptBytes))
	if err != nil {
		return "", fmt.Errorf("upstream: read transcript body: %w", err)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("upstream: parse transcript: %w (body %q)", err, truncate(string(raw), 200))
	}
	return out.Text, nil
}

// escapeQuotes escapes a filename for a quoted Content-Disposition parameter.
// Filenames come from the client's multipart upload, so a quote or backslash
// must not be able to end the parameter early.
func escapeQuotes(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// LooksLikeMPEG reports whether data begins with an ID3v2 tag or a plausible
// MPEG audio frame header. It is a sniff, not a decoder: the gateway only
// needs enough confidence to type the multipart part.
//
// Both markers are accepted only at offset 0. An MP3 file starts with one or
// the other; scanning a prefix for a frame sync would eventually match noise
// inside a WAV's PCM data and mislabel it, which is exactly the silent
// mis-serve the sniff exists to avoid.
func LooksLikeMPEG(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	if bytes.HasPrefix(data, []byte("ID3")) {
		return true
	}
	return isMPEGFrameHeader(data[0], data[1], data[2], data[3])
}

// isMPEGFrameHeader validates the four-byte MPEG audio frame header at b.
func isMPEGFrameHeader(b0, b1, b2, b3 byte) bool {
	if b0 != 0xFF || b1&0xE0 != 0xE0 {
		return false
	}
	version := (b1 >> 3) & 0x03 // 0 = MPEG 2.5, 1 = reserved, 2 = MPEG-2, 3 = MPEG-1
	if version == 1 {
		return false
	}
	layer := (b1 >> 1) & 0x03 // 0 = reserved
	if layer == 0 {
		return false
	}
	bitrate := (b2 >> 4) & 0x0F // 0 = free format, 15 = bad
	if bitrate == 0 || bitrate == 15 {
		return false
	}
	sampleRate := (b2 >> 2) & 0x03 // 3 = reserved
	if sampleRate == 3 {
		return false
	}
	emphasis := b3 & 0x03 // 2 = reserved
	return emphasis != 2
}
