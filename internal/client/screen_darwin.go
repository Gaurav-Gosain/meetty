//go:build darwin

package client

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os/exec"
	"strconv"
)

// RawFrame holds raw pixel data from screen capture.
type RawFrame struct {
	Pixels []byte
	Width  int
	Height int
	Stride int
	Format uint32
}

// PipeWire SPA video format constants (matching screen_linux.go for cross-platform compatibility).
const (
	spaVideoFormatRGBx = 7
	spaVideoFormatBGRx = 8
	spaVideoFormatXRGB = 9
	spaVideoFormatXBGR = 10
	spaVideoFormatRGBA = 11
	spaVideoFormatBGRA = 12
	spaVideoFormatARGB = 13
	spaVideoFormatABGR = 14
	spaVideoFormatRGB  = 15
	spaVideoFormatBGR  = 16
)

const screenMaxWidth = ScreenMaxWidth

// CaptureScreen captures the screen on macOS using ffmpeg avfoundation.
// Sends PNG-encoded frames on frameCh.
func CaptureScreen(ctx context.Context, fps int, frameCh chan<- []byte) error {
	if fps <= 0 {
		fps = ScreenFPS
	}

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-f", "avfoundation",
		"-framerate", strconv.Itoa(fps),
		"-capture_cursor", "1",
		"-i", "1:none",
		"-vf", "scale=1920:1080:force_original_aspect_ratio=decrease",
		"-vcodec", "png",
		"-f", "image2pipe",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg screen capture: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	scanner.Split(splitPNG)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			cmd.Process.Kill()
			return ctx.Err()
		default:
		}

		frame := scanner.Bytes()
		if len(frame) == 0 {
			continue
		}

		if _, err := png.DecodeConfig(bytes.NewReader(frame)); err != nil {
			continue
		}

		frameCopy := make([]byte, len(frame))
		copy(frameCopy, frame)

		select {
		case frameCh <- frameCopy:
		default:
		}
	}

	return cmd.Wait()
}

// CaptureScreenRaw captures the screen and sends raw RawFrame structs.
// On macOS, this uses ffmpeg to capture PNG frames, decodes them, and
// converts to raw RGBA pixel data for the VP8 encoding pipeline.
func CaptureScreenRaw(ctx context.Context, fps int, frameCh chan<- RawFrame) error {
	if fps <= 0 {
		fps = ScreenFPS
	}

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-f", "avfoundation",
		"-framerate", strconv.Itoa(fps),
		"-capture_cursor", "1",
		"-i", "1:none",
		"-vf", "scale=1920:1080:force_original_aspect_ratio=decrease",
		"-vcodec", "png",
		"-f", "image2pipe",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg screen capture: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	scanner.Split(splitPNG)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			cmd.Process.Kill()
			return ctx.Err()
		default:
		}

		data := scanner.Bytes()
		if len(data) == 0 {
			continue
		}

		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			continue
		}

		frame := imageToRawFrame(img)
		if frame.Pixels == nil {
			continue
		}

		select {
		case frameCh <- frame:
		default:
		}
	}

	return cmd.Wait()
}

// imageToRawFrame converts a Go image to a RawFrame with RGBA format.
func imageToRawFrame(img image.Image) RawFrame {
	bounds := img.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()
	if w <= 0 || h <= 0 {
		return RawFrame{}
	}

	stride := w * 4
	pixels := make([]byte, stride*h)

	for y := range h {
		for x := range w {
			r, g, b, a := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			off := y*stride + x*4
			pixels[off+0] = uint8(r >> 8)
			pixels[off+1] = uint8(g >> 8)
			pixels[off+2] = uint8(b >> 8)
			pixels[off+3] = uint8(a >> 8)
		}
	}

	return RawFrame{
		Pixels: pixels,
		Width:  w,
		Height: h,
		Stride: stride,
		Format: spaVideoFormatRGBA,
	}
}

// RawFrameToRGB converts a raw frame to packed RGB bytes.
func RawFrameToRGB(frame RawFrame) (rgb []byte, w, h int) {
	srcW, srcH := frame.Width, frame.Height
	if srcW <= 0 || srcH <= 0 || len(frame.Pixels) < srcH*frame.Stride {
		return nil, 0, 0
	}

	dstW, dstH := srcW, srcH
	if dstW > screenMaxWidth {
		dstH = dstH * screenMaxWidth / dstW
		dstW = screenMaxWidth
	}

	bpp := 4
	if frame.Format == spaVideoFormatRGB || frame.Format == spaVideoFormatBGR {
		bpp = 3
	}

	rgb = make([]byte, dstW*dstH*3)

	for dy := range dstH {
		sy := dy * srcH / dstH
		for dx := range dstW {
			sx := dx * srcW / dstW
			s := sy*frame.Stride + sx*bpp
			d := (dy*dstW + dx) * 3

			switch frame.Format {
			case spaVideoFormatBGRx, spaVideoFormatBGRA:
				rgb[d+0] = frame.Pixels[s+2]
				rgb[d+1] = frame.Pixels[s+1]
				rgb[d+2] = frame.Pixels[s+0]
			case spaVideoFormatRGBx, spaVideoFormatRGBA:
				rgb[d+0] = frame.Pixels[s+0]
				rgb[d+1] = frame.Pixels[s+1]
				rgb[d+2] = frame.Pixels[s+2]
			case spaVideoFormatXRGB, spaVideoFormatARGB:
				rgb[d+0] = frame.Pixels[s+1]
				rgb[d+1] = frame.Pixels[s+2]
				rgb[d+2] = frame.Pixels[s+3]
			case spaVideoFormatXBGR, spaVideoFormatABGR:
				rgb[d+0] = frame.Pixels[s+3]
				rgb[d+1] = frame.Pixels[s+2]
				rgb[d+2] = frame.Pixels[s+1]
			case spaVideoFormatRGB:
				rgb[d+0] = frame.Pixels[s+0]
				rgb[d+1] = frame.Pixels[s+1]
				rgb[d+2] = frame.Pixels[s+2]
			case spaVideoFormatBGR:
				rgb[d+0] = frame.Pixels[s+2]
				rgb[d+1] = frame.Pixels[s+1]
				rgb[d+2] = frame.Pixels[s+0]
			default:
				return nil, 0, 0
			}
		}
	}

	return rgb, dstW, dstH
}

// RawFrameToYCbCr420 converts a raw frame directly to YCbCr 4:2:0 for VP8 encoding.
func RawFrameToYCbCr420(frame RawFrame) *image.YCbCr {
	srcW, srcH := frame.Width, frame.Height
	if srcW <= 0 || srcH <= 0 || len(frame.Pixels) < srcH*frame.Stride {
		return nil
	}

	dstW, dstH := srcW, srcH
	if dstW > screenMaxWidth {
		dstH = dstH * screenMaxWidth / dstW
		dstW = screenMaxWidth
	}

	bpp := 4
	if frame.Format == spaVideoFormatRGB || frame.Format == spaVideoFormatBGR {
		bpp = 3
	}

	out := image.NewYCbCr(image.Rect(0, 0, dstW, dstH), image.YCbCrSubsampleRatio420)

	for dy := range dstH {
		sy := dy * srcH / dstH
		for dx := range dstW {
			sx := dx * srcW / dstW
			s := sy*frame.Stride + sx*bpp

			var r, g, b byte
			switch frame.Format {
			case spaVideoFormatBGRx, spaVideoFormatBGRA:
				r, g, b = frame.Pixels[s+2], frame.Pixels[s+1], frame.Pixels[s+0]
			case spaVideoFormatRGBx, spaVideoFormatRGBA:
				r, g, b = frame.Pixels[s+0], frame.Pixels[s+1], frame.Pixels[s+2]
			case spaVideoFormatXRGB, spaVideoFormatARGB:
				r, g, b = frame.Pixels[s+1], frame.Pixels[s+2], frame.Pixels[s+3]
			case spaVideoFormatXBGR, spaVideoFormatABGR:
				r, g, b = frame.Pixels[s+3], frame.Pixels[s+2], frame.Pixels[s+1]
			case spaVideoFormatRGB:
				r, g, b = frame.Pixels[s+0], frame.Pixels[s+1], frame.Pixels[s+2]
			case spaVideoFormatBGR:
				r, g, b = frame.Pixels[s+2], frame.Pixels[s+1], frame.Pixels[s+0]
			default:
				return nil
			}

			yy, cb, cr := color.RGBToYCbCr(r, g, b)
			out.Y[dy*out.YStride+dx] = yy

			if dx%2 == 0 && dy%2 == 0 {
				ci := (dy/2)*out.CStride + (dx / 2)
				out.Cb[ci] = cb
				out.Cr[ci] = cr
			}
		}
	}

	return out
}

// splitPNG is a bufio.SplitFunc that splits on PNG boundaries.
func splitPNG(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	pngMagic := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	iendMarker := []byte{0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}

	start := bytes.Index(data, pngMagic)
	if start == -1 {
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	}

	searchFrom := start + len(pngMagic)
	iendIdx := bytes.Index(data[searchFrom:], iendMarker)
	if iendIdx == -1 {
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	}

	end := searchFrom + iendIdx + len(iendMarker)
	return end, data[start:end], nil
}

