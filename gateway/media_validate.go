package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Limits from PSLLAMA-INTEGRATION-PLAN.md §2.2 (U-10). Checked before anything is queued.
const (
	mediaValMaxImageBytes     = 8 << 20
	mediaValMaxAudioBytes     = 10 << 20
	mediaValMaxAudioSeconds   = 60
	mediaValMaxReferenceBytes = 2 << 20
	mediaValMaxRefImages      = 4
	mediaValMaxPromptChars    = 2000
	mediaValMaxTTSChars       = 1000
	mediaValMaxVideoFrames    = 33
)

const (
	mediaValCodeInvalid  = "invalid"
	mediaValCodeTooLarge = "too_large"
	mediaValCodeTooLong  = "too_long"
	mediaValCodeConsent  = "consent_required"
)

// mediaValError carries the field name and a fixed message. It never echoes the input value.
type mediaValError struct {
	Code    string
	Field   string
	Message string
}

func (e *mediaValError) Error() string { return e.Field + " " + e.Message }

func mediaValFail(code, field, message string) error {
	return &mediaValError{Code: code, Field: field, Message: message}
}

type mediaValImageRequest struct {
	Prompt       string   `json:"prompt"`
	Model        string   `json:"model"`
	Width        int      `json:"width"`
	Height       int      `json:"height"`
	InitImageB64 string   `json:"init_image_b64"`
	RefImagesB64 []string `json:"ref_images_b64"`
}

type mediaValVideoRequest struct {
	Prompt       string `json:"prompt"`
	Model        string `json:"model"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Frames       int    `json:"frames"`
	InitImageB64 string `json:"init_image_b64"`
}

type mediaValTranscribeRequest struct {
	AudioB64 string `json:"audio_b64"`
}

type mediaValSpeechRequest struct {
	Input      string `json:"input"`
	Model      string `json:"model"`
	SpeakerB64 string `json:"speaker_b64"`
	Consent    bool   `json:"consent"`
}

type mediaValVoiceRequest struct {
	AudioB64   string `json:"audio_b64"`
	SpeakerB64 string `json:"speaker_b64"`
	Consent    bool   `json:"consent"`
}

func (r mediaValImageRequest) validate(allowedModels map[string]bool) error {
	if err := mediaValText("prompt", r.Prompt, mediaValMaxPromptChars); err != nil {
		return err
	}
	if err := mediaValModel(r.Model, allowedModels); err != nil {
		return err
	}
	if err := mediaValDims(r.Width, r.Height, 64, 256, 1024); err != nil {
		return err
	}
	if len(r.RefImagesB64) > mediaValMaxRefImages {
		return mediaValFail(mediaValCodeInvalid, "ref_images_b64", fmt.Sprintf("allows at most %d images", mediaValMaxRefImages))
	}
	if err := mediaValOptionalImage("init_image_b64", r.InitImageB64); err != nil {
		return err
	}
	for i, ref := range r.RefImagesB64 {
		if err := mediaValImage(fmt.Sprintf("ref_images_b64[%d]", i), ref); err != nil {
			return err
		}
	}
	return nil
}

func (r mediaValVideoRequest) validate(allowedModels map[string]bool) error {
	if err := mediaValText("prompt", r.Prompt, mediaValMaxPromptChars); err != nil {
		return err
	}
	if err := mediaValModel(r.Model, allowedModels); err != nil {
		return err
	}
	if err := mediaValDims(r.Width, r.Height, 16, 128, 1280); err != nil {
		return err
	}
	// Frame counts must be 4k+1 (1, 5, 9, ..., 33) under the gateway's stricter cap.
	if r.Frames < 1 || r.Frames > mediaValMaxVideoFrames || r.Frames%4 != 1 {
		return mediaValFail(mediaValCodeInvalid, "frames", fmt.Sprintf("must be 4k+1 and at most %d", mediaValMaxVideoFrames))
	}
	return mediaValOptionalImage("init_image_b64", r.InitImageB64)
}

func (r mediaValTranscribeRequest) validate() error {
	return mediaValAudio("audio_b64", r.AudioB64)
}

func (r mediaValSpeechRequest) validate(allowedModels map[string]bool) error {
	if err := mediaValText("input", r.Input, mediaValMaxTTSChars); err != nil {
		return err
	}
	if err := mediaValModel(r.Model, allowedModels); err != nil {
		return err
	}
	return mediaValReferenceVoice(r.SpeakerB64, r.Consent)
}

func (r mediaValVoiceRequest) validate() error {
	if err := mediaValAudio("audio_b64", r.AudioB64); err != nil {
		return err
	}
	return mediaValReferenceVoice(r.SpeakerB64, r.Consent)
}

// mediaValDecode strictly decodes standard base64 and bounds the size before and after decoding.
func mediaValDecode(field, encoded string, maxBytes int) ([]byte, error) {
	if encoded == "" {
		return nil, mediaValFail(mediaValCodeInvalid, field, "is required")
	}
	if len(encoded) > (maxBytes+2)/3*4 {
		return nil, mediaValFail(mediaValCodeTooLarge, field, "exceeds the size limit")
	}
	// Strict mode still skips CR/LF, so reject them explicitly.
	if strings.ContainsAny(encoded, "\r\n") {
		return nil, mediaValFail(mediaValCodeInvalid, field, "must be standard base64")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, mediaValFail(mediaValCodeInvalid, field, "must be standard base64")
	}
	if len(data) > maxBytes {
		return nil, mediaValFail(mediaValCodeTooLarge, field, "exceeds the size limit")
	}
	return data, nil
}

func mediaValIsPNG(data []byte) bool {
	return bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
}

func mediaValIsJPEG(data []byte) bool {
	return len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF
}

func mediaValImage(field, encoded string) error {
	data, err := mediaValDecode(field, encoded, mediaValMaxImageBytes)
	if err != nil {
		return err
	}
	if !mediaValIsPNG(data) && !mediaValIsJPEG(data) {
		return mediaValFail(mediaValCodeInvalid, field, "must be a PNG or JPEG image")
	}
	return nil
}

func mediaValOptionalImage(field, encoded string) error {
	if encoded == "" {
		return nil
	}
	return mediaValImage(field, encoded)
}

// mediaValWAVDuration returns the duration from the RIFF/WAVE header. A declared data size larger
// than the payload is clamped to the payload, so the duration never exceeds the bytes received.
func mediaValWAVDuration(data []byte) (float64, bool) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, false
	}
	n := int64(len(data))
	var byteRate uint32
	var dataBytes int64 = -1
	haveRate := false
	for off := int64(12); off+8 <= n && (!haveRate || dataBytes < 0); {
		id := string(data[off : off+4])
		size := int64(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			if size < 12 || body+12 > n {
				return 0, false
			}
			byteRate = binary.LittleEndian.Uint32(data[body+8 : body+12])
			haveRate = true
		case "data":
			dataBytes = min(size, n-body)
		}
		off = body + size + (size & 1)
	}
	if !haveRate || byteRate == 0 || dataBytes <= 0 {
		return 0, false
	}
	return float64(dataBytes) / float64(byteRate), true
}

func mediaValAudio(field, encoded string) error {
	data, err := mediaValDecode(field, encoded, mediaValMaxAudioBytes)
	if err != nil {
		return err
	}
	seconds, ok := mediaValWAVDuration(data)
	if !ok {
		return mediaValFail(mediaValCodeInvalid, field, "must be a WAV file")
	}
	if seconds > mediaValMaxAudioSeconds {
		return mediaValFail(mediaValCodeTooLong, field, fmt.Sprintf("exceeds %d seconds", mediaValMaxAudioSeconds))
	}
	return nil
}

// mediaValReferenceVoice applies to any request carrying a reference voice, which is voice cloning.
func mediaValReferenceVoice(encoded string, consent bool) error {
	if encoded == "" {
		return nil
	}
	if !consent {
		return mediaValFail(mediaValCodeConsent, "consent", "is required for voice cloning")
	}
	data, err := mediaValDecode("speaker_b64", encoded, mediaValMaxReferenceBytes)
	if err != nil {
		return err
	}
	if _, ok := mediaValWAVDuration(data); !ok {
		return mediaValFail(mediaValCodeInvalid, "speaker_b64", "must be a WAV file")
	}
	return nil
}

func mediaValText(field, text string, maxChars int) error {
	if strings.TrimSpace(text) == "" {
		return mediaValFail(mediaValCodeInvalid, field, "is required")
	}
	if !utf8.ValidString(text) {
		return mediaValFail(mediaValCodeInvalid, field, "must be valid UTF-8")
	}
	if utf8.RuneCountInString(text) > maxChars {
		return mediaValFail(mediaValCodeTooLong, field, fmt.Sprintf("exceeds %d characters", maxChars))
	}
	return nil
}

func mediaValDims(width, height, step, lo, hi int) error {
	for _, dim := range []struct {
		name  string
		value int
	}{{"width", width}, {"height", height}} {
		if dim.value < lo || dim.value > hi || dim.value%step != 0 {
			return mediaValFail(mediaValCodeInvalid, dim.name, fmt.Sprintf("must be a multiple of %d from %d to %d", step, lo, hi))
		}
	}
	return nil
}

// mediaValModel accepts an empty model (the PS5 applies its default) or an exact allow-list entry.
// Path separators and ".." are rejected before the lookup so traversal attempts get the same answer.
func mediaValModel(model string, allowed map[string]bool) error {
	if model == "" {
		return nil
	}
	if strings.Contains(model, "..") || strings.ContainsAny(model, `/\`) || !allowed[model] {
		return mediaValFail(mediaValCodeInvalid, "model", "is not an allowed model")
	}
	return nil
}
