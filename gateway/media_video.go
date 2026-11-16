package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// mediaVideoModels is the video model allow-list. HunyuanVideo is left out until the licence choice (M-3) is made.
var mediaVideoModels = map[string]bool{"wan2.1-t2v-1.3b": true}

const (
	mediaVideoDefaultModel = "wan2.1-t2v-1.3b"
	mediaVideoDefaultFPS   = 16 // used when PSllama reports no usable fps
	mediaVideoMaxBodyBytes = 16 << 20
	mediaVideoResultPrefix = "/v1/ai/jobs/"
	mediaVideoResultSuffix = "/result"
)

// mediaVideoFormats maps the accepted output format to its content type.
var mediaVideoFormats = map[string]string{"mp4": "video/mp4", "webm": "video/webm"}

// mediaVideoInput is the POST body. Format is gateway-only and is not forwarded to PSllama.
type mediaVideoInput struct {
	mediaValVideoRequest
	Format string `json:"format"`
}

// mediaVideoPS5Request is the body PSllama receives.
type mediaVideoPS5Request struct {
	Prompt       string `json:"prompt"`
	Model        string `json:"model"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Frames       int    `json:"frames"`
	InitImageB64 string `json:"init_image_b64,omitempty"`
}

// handleVideoGenerate serves POST /v1/videos/generations. The job runs on lifetime, not on the request, and the
// response is 202 with the job ID. Poll GET /v1/ai/jobs/{id}, then fetch the bytes once from the result route.
func (g *Gateway) handleVideoGenerate(lifetime context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subject := principalFromRequest(r)
		if subject == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mediaVideoMaxBodyBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid video request")
			return
		}
		var in mediaVideoInput
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid video request")
			return
		}
		if in.Model == "" {
			in.Model = mediaVideoDefaultModel
		}
		if in.Format == "" {
			in.Format = "mp4"
		}
		if _, ok := mediaVideoFormats[in.Format]; !ok {
			writeError(w, http.StatusBadRequest, "format must be mp4 or webm")
			return
		}
		if err := in.validate(mediaVideoModels); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		body, err := json.Marshal(mediaVideoPS5Request{Prompt: in.Prompt, Model: in.Model, Width: in.Width,
			Height: in.Height, Frames: in.Frames, InitImageB64: in.InitImageB64})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "AI media request failed")
			return
		}
		spec := mediaJobSpec{Route: "/v1/videos/generations", Permission: quotaPermissionVideo, QuotaAmount: 1,
			Model: in.Model, InBytes: int64(len(raw))}
		run := func(ctx context.Context, job *queueJob) error {
			res, err := g.ps5.Media(ctx, "video", ProviderRequest(body))
			if err != nil {
				return err
			}
			frames, fps, err := mediaVideoFrames(res.Body)
			if err != nil {
				return err
			}
			out, err := videoEncode(ctx, videoEncodeOptions{Format: in.Format, FPS: fps, MaxFrames: mediaValMaxVideoFrames}, frames)
			if err != nil {
				return err
			}
			mediaSetResult(ctx, MediaResult{ContentType: mediaVideoFormats[in.Format], Body: out})
			return nil
		}
		id, err := g.startMediaJob(lifetime, subject, spec, run)
		if err != nil {
			var fail *mediaRunFailure
			if errors.As(err, &fail) {
				fail.write(w)
				return
			}
			writeError(w, http.StatusInternalServerError, "AI queue is unavailable")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
	}
}

// mediaVideoFrames extracts the base64 PNG frames and the frame rate from a PSllama video response.
func mediaVideoFrames(body []byte) ([][]byte, int, error) {
	var resp struct {
		FPS  int `json:"fps"`
		Data []struct {
			B64PNG string `json:"b64_png"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Data) == 0 {
		return nil, 0, &ps5Error{Message: "PS5 video response was invalid"}
	}
	frames := make([][]byte, 0, len(resp.Data))
	for _, d := range resp.Data {
		frame, err := base64.StdEncoding.DecodeString(d.B64PNG)
		if err != nil {
			return nil, 0, &ps5Error{Message: "PS5 video response was invalid"}
		}
		frames = append(frames, frame)
	}
	fps := resp.FPS
	if fps < 1 || fps > videoEncodeMaxFPS {
		fps = mediaVideoDefaultFPS
	}
	return frames, fps, nil
}

// handleVideoResult serves GET /v1/ai/jobs/{id}/result. The bytes are returned once, and only to the owning subject.
// Every other request gets the same 404 body as an unknown job.
func (g *Gateway) handleVideoResult() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subject := principalFromRequest(r)
		if subject == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		rest, okPrefix := strings.CutPrefix(r.URL.Path, mediaVideoResultPrefix)
		id, okSuffix := strings.CutSuffix(rest, mediaVideoResultSuffix)
		if !okPrefix || !okSuffix || !aiJobIDPattern.MatchString(id) {
			writeError(w, http.StatusNotFound, aiJobNotFound)
			return
		}
		res, found := mediaResults.Take(id, subject)
		if !found {
			writeError(w, http.StatusNotFound, aiJobNotFound)
			return
		}
		w.Header().Set("Content-Type", res.ContentType)
		w.Header().Set("Content-Disposition", "inline")
		w.Header().Set("Content-Length", strconv.Itoa(len(res.Body)))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(res.Body)
	}
}
