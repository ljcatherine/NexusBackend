package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// ttsModel is the PSllama TTS model folder. The client cannot choose a model.
const ttsModel = "qwen3-tts-1.7b"

// ttsMaxBodyBytes bounds the JSON body: 2 MiB of reference WAV is about 2.8 MB of base64, plus 1,000 input chars.
const ttsMaxBodyBytes = 4 << 20

// ttsMaxLanguageChars bounds the optional language tag, such as "en" or "en-US".
const ttsMaxLanguageChars = 16

// ttsRequest is the client body. Unknown fields are ignored and never forwarded.
type ttsRequest struct {
	Input             string `json:"input"`
	VoiceReferenceB64 string `json:"voice_reference_b64"`
	Consent           bool   `json:"consent"`
	Language          string `json:"language"`
}

// ttsPS5Body is the body sent to the PS5 speech route. It is built here, so the client's fields pass only
// through validation.
type ttsPS5Body struct {
	Input          string `json:"input"`
	Model          string `json:"model"`
	Language       string `json:"language,omitempty"`
	SpeakerB64     string `json:"speaker_b64,omitempty"`
	ResponseFormat string `json:"response_format"`
}

// handleSpeech serves POST /v1/audio/speech and returns audio/wav. A reference voice is voice cloning, which needs
// ai.voice.clone and consent:true. The reference audio is forwarded to the PS5 and never stored.
func (g *Gateway) handleSpeech() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subject := principalFromRequest(r)
		permissions := g.cfg.SubjectPermissions[subject]
		if !permissions[quotaPermissionAudio] {
			writeError(w, http.StatusForbidden, "permission denied")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ttsMaxBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return
		}
		var req ttsRequest
		if json.Unmarshal(raw, &req) != nil {
			writeError(w, http.StatusBadRequest, "request body is not valid JSON")
			return
		}
		route := "/v1/audio/speech"
		if req.VoiceReferenceB64 != "" {
			if !permissions[quotaPermissionVoiceClone] {
				writeError(w, http.StatusForbidden, "voice cloning requires the ai.voice.clone permission")
				return
			}
			route += "#clone"
		}
		// Validation runs before the runner, so nothing invalid is queued.
		if err := (mediaValSpeechRequest{Input: req.Input, SpeakerB64: req.VoiceReferenceB64, Consent: req.Consent}).validate(nil); err != nil {
			var ve *mediaValError
			if errors.As(err, &ve) && ve.Code == mediaValCodeConsent {
				writeError(w, http.StatusForbidden, err.Error())
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !ttsLanguageValid(req.Language) {
			writeError(w, http.StatusBadRequest, "language must be a short tag of letters and hyphens")
			return
		}
		body, err := json.Marshal(ttsPS5Body{Input: req.Input, Model: ttsModel, Language: req.Language,
			SpeakerB64: req.VoiceReferenceB64, ResponseFormat: "wav"})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "AI request could not be built")
			return
		}
		spec := mediaJobSpec{Route: route, Permission: quotaPermissionAudio, QuotaAmount: 1, Model: ttsModel, InBytes: int64(len(raw))}
		var out MediaResult
		run := func(ctx context.Context, job *queueJob) error {
			res, err := g.ps5.Media(ctx, "speech", body)
			if err != nil {
				return err
			}
			if !ttsIsWAV(res.Body) {
				return &ps5Error{Status: http.StatusBadGateway, Message: "PS5 returned no WAV audio"}
			}
			out = res
			return nil
		}
		if !g.runMediaJob(w, r, spec, run) {
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out.Body)
	}
}

// ttsIsWAV reports whether data starts with a RIFF/WAVE header.
func ttsIsWAV(data []byte) bool {
	return len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WAVE"
}

// ttsLanguageValid accepts an empty tag or up to ttsMaxLanguageChars letters and hyphens.
func ttsLanguageValid(tag string) bool {
	if len(tag) > ttsMaxLanguageChars {
		return false
	}
	for _, c := range tag {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}
