//go:build !linux && !darwin

package client

import (
	"context"
	"fmt"
	"image"
)

// RawFrame holds raw pixel data from screen capture.
type RawFrame struct {
	Pixels []byte
	Width  int
	Height int
	Stride int
	Format uint32
}

// CaptureScreen is not yet implemented on this platform.
func CaptureScreen(ctx context.Context, fps int, frameCh chan<- []byte) error {
	return fmt.Errorf("screen capture is not supported on this platform yet")
}

// CaptureScreenRaw is not yet implemented on this platform.
func CaptureScreenRaw(ctx context.Context, fps int, frameCh chan<- RawFrame) error {
	return fmt.Errorf("screen capture is not supported on this platform yet")
}

// RawFrameToRGB is a stub for unsupported platforms.
func RawFrameToRGB(frame RawFrame) (rgb []byte, w, h int) {
	return nil, 0, 0
}

// RawFrameToYCbCr420 is a stub for unsupported platforms.
func RawFrameToYCbCr420(frame RawFrame) *image.YCbCr {
	return nil
}
