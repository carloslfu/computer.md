// SPDX-License-Identifier: Apache-2.0

package computer

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/image/draw"
)

// ScreenshotService handles screen capture and image processing.
//
// display/xauth select which X server is captured. Default is the
// shared Xvfb :1 (Pattern A) — what the dashboard shows, UNCHANGED.
// NewScreenshotServiceForDisplay captures a sandbox's private Xvfb :N
// (Pattern B) so a fully-isolated workload can be observed on its own
// display (Phase 5). A private :N Xvfb is spawned with no auth file.
type ScreenshotService struct {
	screenshotDir string
	display       string
	xauth         string
	// Target dimensions for manager computer-use. Must match the
	// display_width_px/display_height_px in the manager tool config.
	targetWidth  int
	targetHeight int
}

// NewScreenshotService creates a screenshot service for the shared
// display (:1). Behavior is byte-identical to before Phase 5.
func NewScreenshotService(dir string) *ScreenshotService {
	return &ScreenshotService{
		screenshotDir: dir,
		display:       ":1",
		xauth:         "/home/vibecraft/.Xauthority",
		targetWidth:   1280,
		targetHeight:  800,
	}
}

// NewScreenshotServiceForDisplay captures a sandbox's private Xvfb :n.
func NewScreenshotServiceForDisplay(dir string, n int) *ScreenshotService {
	return &ScreenshotService{
		screenshotDir: dir,
		display:       fmt.Sprintf(":%d", n),
		targetWidth:   1280,
		targetHeight:  800,
	}
}

// xenv is the X environment for the exec'd scrot/import: DISPLAY always,
// XAUTHORITY only for the auth-protected shared :1.
func (s *ScreenshotService) xenv() []string {
	e := []string{"DISPLAY=" + s.display}
	if s.xauth != "" {
		e = append(e, "XAUTHORITY="+s.xauth)
	}
	return e
}

// CaptureBase64 takes a screenshot, resizes it for the manager, and returns
// a base64-encoded PNG string.
func (s *ScreenshotService) CaptureBase64(ctx context.Context) (string, error) {
	img, err := s.capture(ctx)
	if err != nil {
		return "", err
	}

	// Resize for the manager computer-use API.
	resized := s.smartCrop(img)

	var buf bytes.Buffer
	if err := png.Encode(&buf, resized); err != nil {
		return "", fmt.Errorf("encoding PNG: %w", err)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// CaptureToFile takes a screenshot and saves it to disk, returning the file path.
func (s *ScreenshotService) CaptureToFile(ctx context.Context) (string, error) {
	img, err := s.capture(ctx)
	if err != nil {
		return "", err
	}

	filename := fmt.Sprintf("screenshot_%s.png", time.Now().Format("20060102_150405"))
	path := filepath.Join(s.screenshotDir, filename)

	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("creating screenshot file: %w", err)
	}
	defer f.Close()

	if err := png.Encode(f, img); err != nil {
		return "", fmt.Errorf("writing PNG: %w", err)
	}

	return path, nil
}

// CaptureCropped takes a screenshot of a specific region.
func (s *ScreenshotService) CaptureCropped(ctx context.Context, x, y, w, h int) (string, error) {
	// Use import command with crop geometry.
	tmpFile := filepath.Join(s.screenshotDir, fmt.Sprintf("crop_%d.png", time.Now().UnixNano()))
	defer os.Remove(tmpFile)

	geometry := fmt.Sprintf("%dx%d+%d+%d", w, h, x, y)
	cmd := exec.CommandContext(ctx, "import", "-window", "root", "-crop", geometry, tmpFile)
	cmd.Env = append(os.Environ(), s.xenv()...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("capture crop failed: %w (output: %s)", err, string(output))
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		return "", fmt.Errorf("reading cropped screenshot: %w", err)
	}

	return base64.StdEncoding.EncodeToString(data), nil
}

// capture takes a full-screen screenshot using import (ImageMagick).
func (s *ScreenshotService) capture(ctx context.Context) (image.Image, error) {
	tmpFile := filepath.Join(s.screenshotDir, fmt.Sprintf("tmp_%d.png", time.Now().UnixNano()))
	defer os.Remove(tmpFile)

	// Try scrot first (faster), fall back to import (ImageMagick).
	// DISPLAY must be set explicitly since the daemon runs under systemd
	// which doesn't inherit the desktop session environment.
	cmd := exec.CommandContext(ctx, "scrot", "-o", tmpFile)
	cmd.Env = append(os.Environ(), s.xenv()...)
	if output, err := cmd.CombinedOutput(); err != nil {
		// Fall back to import.
		cmd = exec.CommandContext(ctx, "import", "-window", "root", tmpFile)
		cmd.Env = append(os.Environ(), s.xenv()...)
		if output2, err2 := cmd.CombinedOutput(); err2 != nil {
			return nil, fmt.Errorf("screenshot failed (scrot: %s, import: %s): %w", string(output), string(output2), err2)
		}
	}

	f, err := os.Open(tmpFile)
	if err != nil {
		return nil, fmt.Errorf("opening screenshot: %w", err)
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decoding screenshot: %w", err)
	}

	return img, nil
}

// smartCrop resizes the screenshot to the target dimensions using smart
// aspect-ratio-preserving resize. If the source is larger than the target,
// it downscales while preserving aspect ratio. If the aspect ratio doesn't
// match, it fits within the target bounds (no cropping, no distortion).
func (s *ScreenshotService) smartCrop(img image.Image) image.Image {
	bounds := img.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()

	// If already at or below target size, return as-is.
	if srcW <= s.targetWidth && srcH <= s.targetHeight {
		return img
	}

	// Calculate scale factor to fit within target while preserving aspect ratio.
	scaleW := float64(s.targetWidth) / float64(srcW)
	scaleH := float64(s.targetHeight) / float64(srcH)
	scale := scaleW
	if scaleH < scaleW {
		scale = scaleH
	}

	newW := int(float64(srcW) * scale)
	newH := int(float64(srcH) * scale)

	// High-quality downscale via golang.org/x/image/draw (Catmull-Rom),
	// replacing the unmaintained github.com/disintegration/imaging.
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Src, nil)
	return dst
}

// ListScreenshots returns all screenshot file paths in the screenshot directory,
// sorted by modification time (newest first).
func (s *ScreenshotService) ListScreenshots(limit int) ([]string, error) {
	entries, err := os.ReadDir(s.screenshotDir)
	if err != nil {
		return nil, err
	}

	var paths []string
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".png" {
			continue
		}
		// Skip temporary files.
		if len(entry.Name()) > 4 && entry.Name()[:4] == "tmp_" {
			continue
		}
		if len(entry.Name()) > 5 && entry.Name()[:5] == "crop_" {
			continue
		}
		paths = append(paths, filepath.Join(s.screenshotDir, entry.Name()))
		if limit > 0 && len(paths) >= limit {
			break
		}
	}

	return paths, nil
}
