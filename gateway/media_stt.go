package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
)

// sttModel is the PSllama Whisper model path. The client cannot choose a model. The path contains a slash, so it is
// never passed through mediaValModel, which rejects slashes; it goes to the PS5 only as this server-side constant.
const sttModel = "whisper/ggml-large-v3-turbo-q5_0.bin"

// sttMaxBodyBytes bounds the JSON body: 10 MiB of WAV is about 13.4 MB of base64, plus the small fields.
const sttMaxBodyBytes = 16 << 20

// sttMaxLanguageChars bounds the optional language tag, such as "en" or "en-US".
const sttMaxLanguageChars = 16

// sttRequest is the client body. Unknown fields, such as model, are ignored and never forwarded.
type sttRequest struct {
	AudioB64  string `json:"audio_b64"`
	Language  string `json:"language"`
	Translate bool   `json:"translate"`
}

// sttPS5Body is the body sent to the PS5 transcription route. It is built here from validated fields only.
type sttPS5Body struct {
	AudioB64  string `json:"audio_b64"`
	Model     string `json:"model"`
	Language  string `json:"language,omitempty"`
	Translate bool   `json:"translate,omitempty"`
}

// sttSegment and sttResponse are the response shape. Decoding the PS5 body into sttResponse drops any other field.
type sttSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

type sttResponse struct {
	Text     string       `json:"text"`
	Language string       `json:"language"`
	Segments []sttSegment `json:"segments"`
}

// handleTranscription serves POST /v1/audio/transcriptions. The audio is a WAV file, sent as base64 in JSON.
func (g *Gateway) handleTranscription() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		permissions := g.cfg.SubjectPermissions[principalFromRequest(r)]
		if !permissions[quotaPermissionAudio] {
			writeError(w, http.StatusForbidden, "permission denied")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sttMaxBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return
		}
		var req sttRequest
		if json.Unmarshal(raw, &req) != nil {
			writeError(w, http.StatusBadRequest, "request body is not valid JSON")
			return
		}
		if !sttLanguageValid(req.Language) {
			writeError(w, http.StatusBadRequest, "language must be a short tag of letters and hyphens")
			return
		}
		// Validation runs before the runner, so nothing invalid is queued.
		if err := (mediaValTranscribeRequest{AudioB64: req.AudioB64}).validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		audio, err := base64.StdEncoding.DecodeString(req.AudioB64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "audio_b64 must be standard base64")
			return
		}
		seconds, ok := mediaValWAVDuration(audio)
		if !ok {
			writeError(w, http.StatusBadRequest, "audio_b64 must be a WAV file")
			return
		}
		body, err := json.Marshal(sttPS5Body{AudioB64: req.AudioB64, Model: sttModel, Language: req.Language, Translate: req.Translate})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "AI request could not be built")
			return
		}
		// Quota is charged in whole seconds, rounded up, so a short clip still costs one second.
		spec := mediaJobSpec{Route: "/v1/audio/transcriptions", Permission: quotaPermissionAudio,
			QuotaAmount: int(math.Ceil(seconds)), Model: sttModel, InBytes: int64(len(audio))}
		var out []byte
		run := func(ctx context.Context, _ *queueJob) error {
			res, err := g.ps5.Media(ctx, "transcription", body)
			if err != nil {
				return err
			}
			var parsed sttResponse
			if json.Unmarshal(res.Body, &parsed) != nil {
				return &ps5Error{Message: "PS5 returned an invalid response"}
			}
			if parsed.Segments == nil {
				parsed.Segments = []sttSegment{}
			}
			payload, err := json.Marshal(parsed)
			if err != nil {
				return err
			}
			mediaSetResult(ctx, MediaResult{ContentType: "application/json", Body: payload})
			out = payload
			return nil
		}
		if !g.runMediaJob(w, r, spec, run) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}

// sttLanguageValid accepts an empty tag or up to sttMaxLanguageChars letters and hyphens.
func sttLanguageValid(tag string) bool {
	if len(tag) > sttMaxLanguageChars {
		return false
	}
	for _, c := range tag {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}
