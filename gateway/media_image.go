package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// mediaImageModels is the image model allow-list (plan U-22). shortcut: final list depends on plan decision M-3;
// sd-turbo-fp16 carries a restrictive licence (plan §1.7).
var mediaImageModels = map[string]bool{"sd-turbo-fp16": true, "flux2-klein-4b": true}

const (
	mediaImageMaxRequestBytes  = 12 << 20 // client body cap, as the route cap in plan U-51
	mediaImageMaxResponseBytes = 16 << 20 // decoded PNG cap for a reply
	mediaImageMaxSteps         = 100
)

// mediaImageRequest is the client body. The embedded request holds the fields U-10 validates.
type mediaImageRequest struct {
	mediaValImageRequest
	Steps  *int   `json:"steps"`
	Seed   *int64 `json:"seed"`
	Stream bool   `json:"stream"`
}

// mediaImageBody is the PS5 request. It is built from validated fields only.
type mediaImageBody struct {
	Prompt       string   `json:"prompt"`
	Model        string   `json:"model,omitempty"`
	Width        int      `json:"width"`
	Height       int      `json:"height"`
	Steps        *int     `json:"steps,omitempty"`
	Seed         *int64   `json:"seed,omitempty"`
	InitImageB64 string   `json:"init_image_b64,omitempty"`
	RefImagesB64 []string `json:"ref_images_b64,omitempty"`
	Stream       bool     `json:"stream"`
}

// check runs U-10's image checks, then the steps and seed bounds.
func (r mediaImageRequest) check() error {
	if err := r.mediaValImageRequest.validate(mediaImageModels); err != nil {
		return err
	}
	if r.Steps != nil && (*r.Steps < 1 || *r.Steps > mediaImageMaxSteps) {
		return mediaValFail(mediaValCodeInvalid, "steps", "must be from 1 to 100")
	}
	if r.Seed != nil && *r.Seed < -1 {
		return mediaValFail(mediaValCodeInvalid, "seed", "must be -1 or greater")
	}
	return nil
}

// handleImage serves POST /v1/images/generations. Validation runs before anything is queued, so a bad request uses
// no quota and no PS5 time. stream:true relays the PS5's SSE events; otherwise the reply is {"data":[{"b64_json"}]}.
func (g *Gateway) handleImage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, mediaImageMaxRequestBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var req mediaImageRequest
		if err := decoder.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid image request")
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid image request")
			return
		}
		if err := req.check(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		body, err := json.Marshal(mediaImageBody{Prompt: req.Prompt, Model: req.Model, Width: req.Width, Height: req.Height,
			Steps: req.Steps, Seed: req.Seed, InitImageB64: req.InitImageB64, RefImagesB64: req.RefImagesB64, Stream: req.Stream})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid image request")
			return
		}
		spec := mediaJobSpec{Route: "/v1/images/generations", Permission: quotaPermissionImage, QuotaAmount: 1,
			Model: req.Model, InBytes: int64(len(body))}
		if req.Stream {
			g.mediaImageStream(w, r, spec, body)
			return
		}
		g.mediaImageJSON(w, r, spec, body)
	}
}

// mediaImageJSON runs one image job and writes the first PNG as JSON.
func (g *Gateway) mediaImageJSON(w http.ResponseWriter, r *http.Request, spec mediaJobSpec, body ProviderRequest) {
	var b64 string
	run := func(ctx context.Context, _ *queueJob) error {
		res, err := g.ps5.Media(ctx, "image", body)
		if err != nil {
			return err
		}
		first, err := mediaImageFirstPNG(res.Body)
		if err != nil {
			return err
		}
		mediaSetResult(ctx, res)
		b64 = first
		return nil
	}
	if g.runMediaJob(w, r, spec, run) {
		writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]string{{"b64_json": b64}}})
	}
}

// mediaImageStream relays the PS5's SSE events as they arrive. Refusals before the first event are plain JSON. After
// that, a failure ends the stream with an error event, unless the PS5 already sent one.
func (g *Gateway) mediaImageStream(w http.ResponseWriter, r *http.Request, spec mediaJobSpec, body ProviderRequest) {
	subject := principalFromRequest(r)
	job, fail := g.mediaAdmit(r.Context(), subject, spec)
	if fail != nil {
		fail.write(w)
		return
	}
	started, sawError := false, false
	forward := func(data []byte) error {
		if !started {
			started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache, no-transform")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
		}
		var probe struct {
			Error json.RawMessage `json:"error"`
			Type  string          `json:"type"`
		}
		_ = json.Unmarshal(data, &probe)
		if (len(probe.Error) > 0 && string(probe.Error) != "null") || probe.Type == "error" {
			sawError = true
		}
		mediaImageSSEData(w, data)
		return nil
	}
	run := func(ctx context.Context, _ *queueJob) error {
		return g.ps5.control.postSSE(ctx, g.ps5.control.media, "/v1/images/generations", ps5ImageTimeout, body, forward)
	}
	state, _, fail := g.mediaExecute(r.Context(), job, subject, spec, run, nil)
	switch {
	case fail != nil && !started:
		fail.write(w)
	case fail != nil && !sawError:
		errJSON, _ := json.Marshal(map[string]any{"error": map[string]string{"message": fail.Message}})
		mediaImageSSEData(w, errJSON)
	case state == queueStateDone && !started:
		writeError(w, http.StatusBadGateway, "PS5 returned no image")
	case state == queueStateDone:
		mediaImageSSEData(w, []byte("[DONE]"))
	}
}

// mediaImageFirstPNG returns the first image of a PS5 image reply after checking that it decodes to a PNG within the
// reply cap. The failure is a 502 for the caller.
func mediaImageFirstPNG(body []byte) (string, error) {
	var reply struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &reply) != nil || len(reply.Data) == 0 {
		return "", &ps5Error{Message: "PS5 returned an invalid image"}
	}
	b64 := reply.Data[0].B64
	img, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || !mediaValIsPNG(img) {
		return "", &ps5Error{Message: "PS5 returned an invalid image"}
	}
	if len(img) > mediaImageMaxResponseBytes {
		return "", &ps5Error{Message: fmt.Sprintf("PS5 returned an image larger than %d MiB", mediaImageMaxResponseBytes>>20)}
	}
	return b64, nil
}

// mediaImageSSEData writes one SSE data event and flushes it. Newlines are removed so the event stays on one line.
func mediaImageSSEData(w http.ResponseWriter, data []byte) {
	fmt.Fprintf(w, "data: %s\n\n", bytes.ReplaceAll(data, []byte("\n"), nil))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
