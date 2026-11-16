package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const (
	videoEncodeDefaultMaxFrames = 121
	videoEncodeMaxFPS           = 60
	videoEncodeMaxInputBytes    = 64 << 20
	videoEncodeMaxOutputBytes   = 256 << 20
	videoEncodeTimeout          = 5 * time.Minute
)

// videoEncodeTempRoot is the parent for the per-run frame directory; tests override it to check cleanup.
var videoEncodeTempRoot = os.TempDir()

var videoEncodePNGMagic = []byte("\x89PNG\r\n\x1a\n")

type videoEncodeOptions struct {
	Format    string // "mp4" or "webm"
	FPS       int    // 1..videoEncodeMaxFPS
	MaxFrames int    // 0 means videoEncodeDefaultMaxFrames
}

// videoEncodeArgs builds the ffmpeg argument list. Only the format and fps are caller-influenced, and both are validated here.
func videoEncodeArgs(format string, fps int, inputPattern, outputPath string) ([]string, error) {
	if fps < 1 || fps > videoEncodeMaxFPS {
		return nil, errors.New("video fps out of range")
	}
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-framerate", strconv.Itoa(fps),
		"-i", inputPattern,
		"-pix_fmt", "yuv420p",
	}
	switch format {
	case "mp4":
		args = append(args, "-c:v", "libx264", "-movflags", "+faststart")
	case "webm":
		args = append(args, "-c:v", "libvpx-vp9", "-b:v", "0", "-crf", "34")
	default:
		return nil, errors.New("video format must be mp4 or webm")
	}
	return append(args, outputPath), nil
}

func videoEncodeValidatePNG(frame []byte) error {
	if !bytes.HasPrefix(frame, videoEncodePNGMagic) {
		return errors.New("video frame is not a PNG")
	}
	return nil
}

// videoEncode writes the PNG frames to a private temp dir, encodes them with ffmpeg and returns the output bytes.
// The temp dir is always removed before returning.
func videoEncode(ctx context.Context, opts videoEncodeOptions, frames [][]byte) ([]byte, error) {
	maxFrames := opts.MaxFrames
	if maxFrames == 0 {
		maxFrames = videoEncodeDefaultMaxFrames
	}
	if len(frames) == 0 || len(frames) > maxFrames {
		return nil, fmt.Errorf("video frame count must be 1..%d", maxFrames)
	}
	total := 0
	for _, frame := range frames {
		if err := videoEncodeValidatePNG(frame); err != nil {
			return nil, err
		}
		total += len(frame)
		if total > videoEncodeMaxInputBytes {
			return nil, errors.New("video input exceeds size limit")
		}
	}
	// Build arguments before touching disk so bad format/fps fail fast.
	if _, err := videoEncodeArgs(opts.Format, opts.FPS, "frame-%05d.png", "out"); err != nil {
		return nil, err
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, errors.New("ffmpeg is not available")
	}

	// MkdirTemp creates the directory with mode 0700 on unix.
	dir, err := os.MkdirTemp(videoEncodeTempRoot, "nexus-video-")
	if err != nil {
		return nil, errors.New("video temp directory could not be created")
	}
	defer os.RemoveAll(dir)

	for i, frame := range frames {
		name := filepath.Join(dir, fmt.Sprintf("frame-%05d.png", i))
		if err := os.WriteFile(name, frame, 0o600); err != nil {
			return nil, errors.New("video frame could not be written")
		}
	}
	outName := "out.mp4"
	if opts.Format == "webm" {
		outName = "out.webm"
	}
	outPath := filepath.Join(dir, outName)
	args, err := videoEncodeArgs(opts.Format, opts.FPS, filepath.Join(dir, "frame-%05d.png"), outPath)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, videoEncodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, ffmpeg, args...)
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("video encode timed out")
		}
		return nil, errors.New("video encode failed")
	}

	info, err := os.Stat(outPath)
	if err != nil {
		return nil, errors.New("video encode produced no output")
	}
	if info.Size() > videoEncodeMaxOutputBytes {
		return nil, errors.New("video output exceeds size limit")
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		return nil, errors.New("video output could not be read")
	}
	return out, nil
}
