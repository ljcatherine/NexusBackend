package main

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

var mediaValTestModels = map[string]bool{
	"sd-turbo-fp16":   true,
	"wan2.1-t2v-1.3b": true,
	"qwen3-tts-1.7b":  true,
}

func mediaValTestFile(prefix []byte, total int) []byte {
	data := make([]byte, total)
	copy(data, prefix)
	return data
}

func mediaValTestPNG(total int) []byte {
	return mediaValTestFile([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, total)
}

func mediaValTestJPEG(total int) []byte {
	return mediaValTestFile([]byte{0xFF, 0xD8, 0xFF, 0xE0}, total)
}

// mediaValTestWAV builds a 16 kHz mono 16-bit PCM WAV (byte rate 32000) with dataBytes of payload
// and the given declared data-chunk size.
func mediaValTestWAV(dataBytes int, declared uint32) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataBytes))
	copy(h[8:], "WAVE")
	copy(h[12:], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], 16000)
	binary.LittleEndian.PutUint32(h[28:], 32000)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], declared)
	return append(h, make([]byte, dataBytes)...)
}

// mediaValTestSeconds returns a valid WAV of the given length in seconds.
func mediaValTestSeconds(seconds int) []byte {
	n := seconds * 32000
	return mediaValTestWAV(n, uint32(n))
}

func mediaValTestB64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

type mediaValCase struct {
	name     string
	check    func() error
	wantCode string // empty means the input must validate
}

func runMediaValCases(t *testing.T, cases []mediaValCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.check()
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var verr *mediaValError
			if !errors.As(err, &verr) || verr.Code != c.wantCode {
				t.Fatalf("error = %v, want code %q", err, c.wantCode)
			}
			if strings.Contains(verr.Error(), "test-") {
				t.Fatalf("error message echoes input: %q", verr.Error())
			}
		})
	}
}

func TestMediaValidateImageRequest(t *testing.T) {
	valid := func(mut func(*mediaValImageRequest)) func() error {
		return func() error {
			r := mediaValImageRequest{Prompt: "a lighthouse", Width: 512, Height: 512}
			if mut != nil {
				mut(&r)
			}
			return r.validate(mediaValTestModels)
		}
	}
	runMediaValCases(t, []mediaValCase{
		{name: "valid defaults", check: valid(nil)},
		{name: "dims at lower edge", check: valid(func(r *mediaValImageRequest) { r.Width, r.Height = 256, 256 })},
		{name: "dims at upper edge", check: valid(func(r *mediaValImageRequest) { r.Width, r.Height = 1024, 768 })},
		{name: "width not multiple of 64", check: valid(func(r *mediaValImageRequest) { r.Width = 500 }), wantCode: mediaValCodeInvalid},
		{name: "width below 256", check: valid(func(r *mediaValImageRequest) { r.Width = 192 }), wantCode: mediaValCodeInvalid},
		{name: "height above 1024", check: valid(func(r *mediaValImageRequest) { r.Height = 1088 }), wantCode: mediaValCodeInvalid},
		{name: "empty prompt", check: valid(func(r *mediaValImageRequest) { r.Prompt = "   " }), wantCode: mediaValCodeInvalid},
		{name: "prompt at 2000 chars", check: valid(func(r *mediaValImageRequest) { r.Prompt = strings.Repeat("é", 2000) })},
		{name: "prompt over 2000 chars", check: valid(func(r *mediaValImageRequest) { r.Prompt = strings.Repeat("a", 2001) }), wantCode: mediaValCodeTooLong},
		{name: "model in allow-list", check: valid(func(r *mediaValImageRequest) { r.Model = "sd-turbo-fp16" })},
		{name: "model not in allow-list", check: valid(func(r *mediaValImageRequest) { r.Model = "other-model" }), wantCode: mediaValCodeInvalid},
		{name: "model traversal dotdot", check: valid(func(r *mediaValImageRequest) { r.Model = "../secret" }), wantCode: mediaValCodeInvalid},
		{name: "model forward slash", check: valid(func(r *mediaValImageRequest) { r.Model = "sd-turbo-fp16/x" }), wantCode: mediaValCodeInvalid},
		{name: "model backslash", check: valid(func(r *mediaValImageRequest) { r.Model = `sd-turbo-fp16\x` }), wantCode: mediaValCodeInvalid},
		{name: "init image PNG", check: valid(func(r *mediaValImageRequest) { r.InitImageB64 = mediaValTestB64(mediaValTestPNG(1024)) })},
		{name: "init image JPEG", check: valid(func(r *mediaValImageRequest) { r.InitImageB64 = mediaValTestB64(mediaValTestJPEG(1024)) })},
		{name: "init image WAV renamed", check: valid(func(r *mediaValImageRequest) { r.InitImageB64 = mediaValTestB64(mediaValTestSeconds(1)) }), wantCode: mediaValCodeInvalid},
		{name: "init image exactly 8 MiB", check: valid(func(r *mediaValImageRequest) { r.InitImageB64 = mediaValTestB64(mediaValTestPNG(8 << 20)) })},
		{name: "init image over 8 MiB", check: valid(func(r *mediaValImageRequest) { r.InitImageB64 = mediaValTestB64(mediaValTestPNG(8<<20 + 1)) }), wantCode: mediaValCodeTooLarge},
		{name: "init image not base64", check: valid(func(r *mediaValImageRequest) { r.InitImageB64 = "!!!not-base64!!!" }), wantCode: mediaValCodeInvalid},
		{name: "init image with newline", check: valid(func(r *mediaValImageRequest) { r.InitImageB64 = mediaValTestB64(mediaValTestPNG(64)) + "\n" }), wantCode: mediaValCodeInvalid},
		{name: "four ref images", check: valid(func(r *mediaValImageRequest) {
			r.RefImagesB64 = []string{mediaValTestB64(mediaValTestPNG(64)), mediaValTestB64(mediaValTestPNG(64)), mediaValTestB64(mediaValTestPNG(64)), mediaValTestB64(mediaValTestPNG(64))}
		})},
		{name: "five ref images", check: valid(func(r *mediaValImageRequest) {
			for i := 0; i < 5; i++ {
				r.RefImagesB64 = append(r.RefImagesB64, mediaValTestB64(mediaValTestPNG(64)))
			}
		}), wantCode: mediaValCodeInvalid},
		{name: "ref image not an image", check: valid(func(r *mediaValImageRequest) { r.RefImagesB64 = []string{mediaValTestB64([]byte("plain text"))} }), wantCode: mediaValCodeInvalid},
	})
}

func TestMediaValidateVideoRequest(t *testing.T) {
	valid := func(mut func(*mediaValVideoRequest)) func() error {
		return func() error {
			r := mediaValVideoRequest{Prompt: "waves", Width: 512, Height: 384, Frames: 33}
			if mut != nil {
				mut(&r)
			}
			return r.validate(mediaValTestModels)
		}
	}
	runMediaValCases(t, []mediaValCase{
		{name: "valid 512x384 33 frames", check: valid(nil)},
		{name: "one frame", check: valid(func(r *mediaValVideoRequest) { r.Frames = 1 })},
		{name: "nine frames", check: valid(func(r *mediaValVideoRequest) { r.Frames = 9 })},
		{name: "frames 0", check: valid(func(r *mediaValVideoRequest) { r.Frames = 0 }), wantCode: mediaValCodeInvalid},
		{name: "frames 8 not 4k+1", check: valid(func(r *mediaValVideoRequest) { r.Frames = 8 }), wantCode: mediaValCodeInvalid},
		{name: "frames 37 over cap", check: valid(func(r *mediaValVideoRequest) { r.Frames = 37 }), wantCode: mediaValCodeInvalid},
		{name: "frames negative", check: valid(func(r *mediaValVideoRequest) { r.Frames = -3 }), wantCode: mediaValCodeInvalid},
		{name: "width not multiple of 16", check: valid(func(r *mediaValVideoRequest) { r.Width = 500 }), wantCode: mediaValCodeInvalid},
		{name: "width above 1280", check: valid(func(r *mediaValVideoRequest) { r.Width = 1296 }), wantCode: mediaValCodeInvalid},
		{name: "height below 128", check: valid(func(r *mediaValVideoRequest) { r.Height = 112 }), wantCode: mediaValCodeInvalid},
		{name: "model traversal", check: valid(func(r *mediaValVideoRequest) { r.Model = `..\..\x` }), wantCode: mediaValCodeInvalid},
		{name: "init image over 8 MiB", check: valid(func(r *mediaValVideoRequest) { r.InitImageB64 = mediaValTestB64(mediaValTestPNG(8<<20 + 1)) }), wantCode: mediaValCodeTooLarge},
	})
}

func TestMediaValidateTranscribeRequest(t *testing.T) {
	audio := func(data []byte) func() error {
		return func() error { return mediaValTranscribeRequest{AudioB64: mediaValTestB64(data)}.validate() }
	}
	runMediaValCases(t, []mediaValCase{
		{name: "valid 5 s WAV", check: audio(mediaValTestSeconds(5))},
		{name: "exactly 60 s", check: audio(mediaValTestSeconds(60))},
		{name: "61 s is too long", check: audio(mediaValTestSeconds(61)), wantCode: mediaValCodeTooLong},
		{name: "PNG renamed as WAV", check: audio(mediaValTestPNG(4096)), wantCode: mediaValCodeInvalid},
		{name: "RIFF but not WAVE", check: audio(append([]byte("RIFF\x00\x00\x00\x00AVI "), make([]byte, 64)...)), wantCode: mediaValCodeInvalid},
		{name: "truncated header", check: audio([]byte("RIFF\x00\x00\x00\x00WAVE")), wantCode: mediaValCodeInvalid},
		{name: "no data chunk", check: audio(mediaValTestWAV(0, 0)[:36]), wantCode: mediaValCodeInvalid},
		{name: "declared size larger than payload uses payload", check: audio(mediaValTestWAV(3*32000, 0xFFFFFFFF))},
		{name: "declared size with 61 s payload is too long", check: audio(mediaValTestWAV(61*32000, 0xFFFFFFFF)), wantCode: mediaValCodeTooLong},
		{name: "over 10 MiB", check: audio(mediaValTestWAV(10<<20+1, uint32(10<<20+1))), wantCode: mediaValCodeTooLarge},
		{name: "empty", check: func() error { return mediaValTranscribeRequest{}.validate() }, wantCode: mediaValCodeInvalid},
	})
}

func TestMediaValidateSpeechRequest(t *testing.T) {
	speech := func(mut func(*mediaValSpeechRequest)) func() error {
		return func() error {
			r := mediaValSpeechRequest{Input: "Hello there."}
			if mut != nil {
				mut(&r)
			}
			return r.validate(mediaValTestModels)
		}
	}
	runMediaValCases(t, []mediaValCase{
		{name: "valid input", check: speech(nil)},
		{name: "empty input", check: speech(func(r *mediaValSpeechRequest) { r.Input = "" }), wantCode: mediaValCodeInvalid},
		{name: "input at 1000 chars", check: speech(func(r *mediaValSpeechRequest) { r.Input = strings.Repeat("a", 1000) })},
		{name: "input over 1000 chars", check: speech(func(r *mediaValSpeechRequest) { r.Input = strings.Repeat("a", 1001) }), wantCode: mediaValCodeTooLong},
		{name: "model traversal", check: speech(func(r *mediaValSpeechRequest) { r.Model = "../qwen3" }), wantCode: mediaValCodeInvalid},
		{name: "speaker without consent", check: speech(func(r *mediaValSpeechRequest) { r.SpeakerB64 = mediaValTestB64(mediaValTestSeconds(3)) }), wantCode: mediaValCodeConsent},
		{name: "speaker with consent", check: speech(func(r *mediaValSpeechRequest) {
			r.SpeakerB64 = mediaValTestB64(mediaValTestSeconds(3))
			r.Consent = true
		})},
		{name: "speaker with consent but PNG", check: speech(func(r *mediaValSpeechRequest) {
			r.SpeakerB64 = mediaValTestB64(mediaValTestPNG(256))
			r.Consent = true
		}), wantCode: mediaValCodeInvalid},
		{name: "speaker over 2 MiB", check: speech(func(r *mediaValSpeechRequest) {
			r.SpeakerB64 = mediaValTestB64(mediaValTestWAV(2<<20, 0))
			r.Consent = true
		}), wantCode: mediaValCodeTooLarge},
	})
}

func TestMediaValidateVoiceRequest(t *testing.T) {
	runMediaValCases(t, []mediaValCase{
		{name: "valid voice turn", check: func() error {
			return mediaValVoiceRequest{AudioB64: mediaValTestB64(mediaValTestSeconds(2))}.validate()
		}},
		{name: "clone without consent", check: func() error {
			return mediaValVoiceRequest{AudioB64: mediaValTestB64(mediaValTestSeconds(2)), SpeakerB64: mediaValTestB64(mediaValTestSeconds(2))}.validate()
		}, wantCode: mediaValCodeConsent},
		{name: "clone with consent", check: func() error {
			return mediaValVoiceRequest{AudioB64: mediaValTestB64(mediaValTestSeconds(2)), SpeakerB64: mediaValTestB64(mediaValTestSeconds(2)), Consent: true}.validate()
		}},
		{name: "voice audio 61 s", check: func() error {
			return mediaValVoiceRequest{AudioB64: mediaValTestB64(mediaValTestSeconds(61))}.validate()
		}, wantCode: mediaValCodeTooLong},
		{name: "voice audio is PNG", check: func() error {
			return mediaValVoiceRequest{AudioB64: mediaValTestB64(mediaValTestPNG(512))}.validate()
		}, wantCode: mediaValCodeInvalid},
	})
}

func TestMediaValidateModelAllowListRejectsEmptyAllowList(t *testing.T) {
	r := mediaValImageRequest{Prompt: "x", Width: 256, Height: 256, Model: "sd-turbo-fp16"}
	if err := r.validate(nil); err == nil {
		t.Fatal("named model accepted with empty allow-list")
	}
}
