# TASK v5: Audio Suite — TTS (/v1/audio/speech) + STT (/v1/audio/transcriptions)

## Ground truth (/root/inception-dig/REPORT.md — read §TTS/STT first)

- `POST /api/tts {text, voice}` → complete `audio/mpeg` body (MPEG-1 L3, 128 kbps,
  44.1 kHz mono). 2.3 s for 31 chars, 10.3 s for 155. Unknown `voice` returns a
  0.44 s clip silently (no error) — so we must discover the legal voice list.
- `POST /api/stt` accepts multipart audio; ONLY real MPEG audio transcribes
  (TTS→STT round trip verified byte-accurate). WAV returns {"text":""} with 200.
- Transport: same clearance cookie + session token + h2 as the chat path
  (`internal/clear`, `internal/upstream` postAuthed pattern).
- `reasoningEffort` enum: high|medium|low|instant (chat path already passes it
  through as a non-standard param; verify nothing breaks).

## Build (in /root/inceptionlabs2api, push to bacteriaw/main)

### 1. TTS: `POST /v1/audio/speech` (OpenAI standard)

Request (OpenAI shape): `{model, input, voice, response_format?, speed?}`.
- `model`: accept anything, map to upstream (single upstream voice engine).
- `input` (the text): required, 400 when empty (structural). Cap? NONE —
  pass-through philosophy: let upstream answer (it will error or truncate itself).
- `voice`: map to upstream voice. FIRST discover the legal voice list:
  - Bundle archaeology: the site's voice picker UI must enumerate voices — find
    the list in the chunks (grep voice names/ids near /api/tts call site).
  - One or two live probes to confirm the list's entries produce distinct audio
    (compare bytes/duration).
  - If discovery fails, map OpenAI's standard voices (alloy/echo/fable/...)
    onto the upstream default and document the mapping honestly in SPEC/README.
- `response_format`: upstream only produces mp3 → honor the field but only
  serve `mp3`; anything else → 400 unsupported_format? NO — pass-through
  philosophy: serve mp3 with Content-Type audio/mpeg regardless, and document
  (client asked for wav? upstream decides what exists; we translate what we
  get). Actually cleaner: accept the param, ignore values other than mp3,
  always respond audio/mpeg. Document in SPEC.
- `speed`: drop silently (upstream has no knob).
- Response: raw audio/mpeg body, 200. Streaming? Upstream sends complete body;
  just proxy it with flushes.

### 2. STT: `POST /v1/audio/transcriptions` (OpenAI standard)

Request: multipart `file` + optional `model`/`language`/`prompt`.
- The upstream needs MPEG audio. Gateway behavior: pass the uploaded bytes
  through to /api/stt as-is. If upstream returns empty text AND the upload was
  not mpeg (sniff the first bytes: ID3/0xFFFB), that is the upstream's answer —
  per pass-through philosophy do NOT transcode in the gateway. Respond with
  {text:""} shaped per OpenAI. BUT document clearly in README that only MP3
  transcribes (the upstream's contract), and note that clients needing other
  formats should convert upstream of us. (No ffmpeg dependency — zero-dep is
  a design win; the report notes WAV is a silent no-op upstream.)
  - Sniffing choice: send file as-is always. If the bytes are identifiably
    MPEG, set the multipart content-type audio/mpeg; otherwise audio/wav.
    That mirrors the site's own client behavior as captured.
- Response: OpenAI shape `{object:"transcription", text: ...}`.

### 3. Models endpoint: add the audio surfaces to /v1/models? NO — OpenAI
doesn't list audio models there. Leave /v1/models as is.

### 4. healthz: counters for tts/stt requests (same counter pattern).

## Method

SDD: SPEC section for both endpoints first (wire specs, voice mapping decision,
pass-through notes), commit. TDD: httptest mocks for upstream /api/tts and
/api/stt (success, empty text, non-mpeg input, upstream 4xx mapping into the
OpenAI error envelope); endpoint tests for OpenAI request/response shapes.
Voice discovery: bundle archaeology first; ≤ 3 live probes to confirm.
Final live smoke authorized: 2 requests (one /v1/audio/speech round trip saving
the mp3 to /tmp, one /v1/audio/transcriptions feeding that mp3 back — assert
text non-empty). Atomic English commits, each green: go build && go vet &&
go test -race ./... . Push to origin main. README (Chinese): new sections for
both endpoints, note the mp3-only transcription contract.
