package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
)

const (
	mediaVoiceMaxBodyBytes    = 16 << 20
	mediaVoiceMaxHistory      = 20
	mediaVoiceMaxHistoryBytes = 16 << 10
)

// mediaVoiceSystemPrompt is the only system prompt the voice engine gets. Clients cannot replace it.
// shortcut: placeholder wording, review before release.
const mediaVoiceSystemPrompt = "You are a voice assistant. Answer briefly in plain sentences that read well when spoken aloud."

type mediaVoiceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// mediaVoiceClientRequest is the client body. Unknown fields, including any system prompt, are ignored.
type mediaVoiceClientRequest struct {
	AudioB64   string              `json:"audio_b64"`
	SpeakerB64 string              `json:"speaker_b64"`
	Consent    bool                `json:"consent"`
	Messages   []mediaVoiceMessage `json:"messages"`
}

// mediaVoicePS5Request is built here for PSllama's /v1/voice/chat. Nothing is copied from the client except audio, speaker and history.
type mediaVoicePS5Request struct {
	AudioB64   string              `json:"audio_b64"`
	SpeakerB64 string              `json:"speaker_b64,omitempty"`
	System     string              `json:"system"`
	Messages   []mediaVoiceMessage `json:"messages"`
}

// handleVoiceChat serves POST /v1/voice/chat as an SSE pass-through of the PS5 voice events.
func (g *Gateway) handleVoiceChat() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mediaVoiceMaxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "voice request is too large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid voice request")
			return
		}
		var input mediaVoiceClientRequest
		if err := json.Unmarshal(raw, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid voice request")
			return
		}
		if err := (mediaValVoiceRequest{AudioB64: input.AudioB64, SpeakerB64: input.SpeakerB64, Consent: input.Consent}).validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		subject := principalFromRequest(r)
		if input.SpeakerB64 != "" && !g.cfg.SubjectPermissions[subject][quotaPermissionVoiceClone] {
			writeError(w, http.StatusForbidden, "permission denied")
			return
		}
		seconds, err := mediaVoiceSeconds(input.AudioB64)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		body := mediaVoicePS5Request{AudioB64: input.AudioB64, SpeakerB64: input.SpeakerB64,
			System: mediaVoiceSystemPrompt, Messages: mediaVoiceHistory(input.Messages)}
		spec := mediaJobSpec{Route: "/v1/voice/chat", Permission: quotaPermissionVoice, QuotaAmount: seconds, InBytes: int64(len(raw))}

		// Headers go out with the first event. A failure before that is a JSON error; after it, the stream's error event is the answer.
		stream := &mediaVoiceStream{ResponseWriter: w}
		run := func(ctx context.Context, job *queueJob) error {
			flusher, _ := w.(http.Flusher)
			err := g.ps5.control.postSSE(ctx, g.ps5.control.media, "/v1/voice/chat", ps5VoiceTimeout, body, func(event []byte) error {
				if !stream.started {
					h := w.Header()
					h.Set("Content-Type", "text/event-stream")
					h.Set("Cache-Control", "no-cache, no-transform")
					h.Set("X-Accel-Buffering", "no")
					w.WriteHeader(http.StatusOK)
					stream.started = true
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
					return err
				}
				if flusher != nil {
					flusher.Flush()
				}
				return nil
			})
			if stream.started {
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			return err
		}
		g.runMediaJob(stream, r, spec, run)
	}
}

// mediaVoiceStream drops header and body writes once the SSE stream has started.
type mediaVoiceStream struct {
	http.ResponseWriter
	started bool
}

func (s *mediaVoiceStream) WriteHeader(code int) {
	if !s.started {
		s.ResponseWriter.WriteHeader(code)
	}
}

func (s *mediaVoiceStream) Write(p []byte) (int, error) {
	if s.started {
		return len(p), nil
	}
	return s.ResponseWriter.Write(p)
}

// mediaVoiceHistory keeps the newest user and assistant turns within the message and byte limits. Older turns and
// other roles are dropped, never rejected.
func mediaVoiceHistory(messages []mediaVoiceMessage) []mediaVoiceMessage {
	kept := make([]mediaVoiceMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "user" || m.Role == "assistant" {
			kept = append(kept, m)
		}
	}
	if len(kept) > mediaVoiceMaxHistory {
		kept = kept[len(kept)-mediaVoiceMaxHistory:]
	}
	total, start := 0, len(kept)
	for start > 0 && total+len(kept[start-1].Content) <= mediaVoiceMaxHistoryBytes {
		start--
		total += len(kept[start].Content)
	}
	return kept[start:]
}

// mediaVoiceSeconds returns the audio length billed to ai.voice, rounded up to whole seconds.
func mediaVoiceSeconds(audioB64 string) (int, error) {
	data, err := mediaValDecode("audio_b64", audioB64, mediaValMaxAudioBytes)
	if err != nil {
		return 0, err
	}
	seconds, ok := mediaValWAVDuration(data)
	if !ok {
		return 0, mediaValFail(mediaValCodeInvalid, "audio_b64", "must be a WAV file")
	}
	return int(math.Ceil(seconds)), nil
}
