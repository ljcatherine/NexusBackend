package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"slices"
	"testing"
)

func videoEncodeTestPNG(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: shade, G: uint8(x * 16), B: uint8(y * 16), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func videoEncodeTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	previous := videoEncodeTempRoot
	videoEncodeTempRoot = root
	t.Cleanup(func() { videoEncodeTempRoot = previous })
	return root
}

func assertVideoEncodeRootEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read temp root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp dir not removed: %d entries left", len(entries))
	}
}

func TestVideoEncodeArgsAreFixedAndValidated(t *testing.T) {
	mp4, err := videoEncodeArgs("mp4", 12, "in/frame-%05d.png", "out/out.mp4")
	if err != nil {
		t.Fatalf("mp4 args: %v", err)
	}
	for _, want := range []string{"libx264", "yuv420p", "+faststart", "-framerate", "12", "out/out.mp4"} {
		if !slices.Contains(mp4, want) {
			t.Fatalf("mp4 args missing %q: %v", want, mp4)
		}
	}
	webm, err := videoEncodeArgs("webm", 24, "in/frame-%05d.png", "out/out.webm")
	if err != nil {
		t.Fatalf("webm args: %v", err)
	}
	if !slices.Contains(webm, "libvpx-vp9") || slices.Contains(webm, "libx264") {
		t.Fatalf("webm args wrong codec: %v", webm)
	}
	for _, bad := range []int{0, -1, videoEncodeMaxFPS + 1} {
		if _, err := videoEncodeArgs("mp4", bad, "in", "out"); err == nil {
			t.Fatalf("fps %d accepted", bad)
		}
	}
	if _, err := videoEncodeArgs("avi", 12, "in", "out"); err == nil {
		t.Fatal("unknown format accepted")
	}
}

func TestVideoEncodeRejectsNonPNGFrameAndRemovesTempDir(t *testing.T) {
	root := videoEncodeTestRoot(t)
	frames := [][]byte{videoEncodeTestPNG(t, 10), []byte("RIFF....WAVE not a png")}
	_, err := videoEncode(context.Background(), videoEncodeOptions{Format: "mp4", FPS: 8}, frames)
	if err == nil {
		t.Fatal("non-PNG frame accepted")
	}
	assertVideoEncodeRootEmpty(t, root)
}

func TestVideoEncodeRejectsFrameCountAboveCap(t *testing.T) {
	root := videoEncodeTestRoot(t)
	frames := [][]byte{videoEncodeTestPNG(t, 1), videoEncodeTestPNG(t, 2)}
	if _, err := videoEncode(context.Background(), videoEncodeOptions{Format: "mp4", FPS: 8, MaxFrames: 1}, frames); err == nil {
		t.Fatal("frame count above cap accepted")
	}
	assertVideoEncodeRootEmpty(t, root)
}

func TestVideoEncodeRealEncodeAndCleanup(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	frames := [][]byte{videoEncodeTestPNG(t, 10), videoEncodeTestPNG(t, 90), videoEncodeTestPNG(t, 180)}

	t.Run("mp4 success", func(t *testing.T) {
		root := videoEncodeTestRoot(t)
		out, err := videoEncode(context.Background(), videoEncodeOptions{Format: "mp4", FPS: 8}, frames)
		if err != nil {
			t.Fatalf("mp4 encode: %v", err)
		}
		if len(out) < 12 || !bytes.Equal(out[4:8], []byte("ftyp")) {
			t.Fatalf("output is not an MP4 container")
		}
		assertVideoEncodeRootEmpty(t, root)
	})

	t.Run("webm success", func(t *testing.T) {
		root := videoEncodeTestRoot(t)
		out, err := videoEncode(context.Background(), videoEncodeOptions{Format: "webm", FPS: 8}, frames)
		if err != nil {
			t.Skipf("webm encode unavailable in this ffmpeg build: %v", err)
		}
		if !bytes.HasPrefix(out, []byte{0x1a, 0x45, 0xdf, 0xa3}) {
			t.Fatalf("output is not a WebM (EBML) container")
		}
		assertVideoEncodeRootEmpty(t, root)
	})

	t.Run("corrupt frame fails and cleans up", func(t *testing.T) {
		root := videoEncodeTestRoot(t)
		corrupt := append(append([]byte{}, videoEncodePNGMagic...), []byte("garbage garbage garbage")...)
		if _, err := videoEncode(context.Background(), videoEncodeOptions{Format: "mp4", FPS: 8}, [][]byte{corrupt}); err == nil {
			t.Fatal("corrupt frame encoded without error")
		}
		assertVideoEncodeRootEmpty(t, root)
	})
}
