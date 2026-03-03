//go:build !linux && !darwin

package client

import (
	"context"
	"fmt"
)

// CameraDevice represents an available webcam.
type CameraDevice struct {
	Path string
	Name string
}

// ListCameras returns nil on unsupported platforms.
func ListCameras() []CameraDevice {
	return nil
}

// CaptureWebcam is not supported on this platform.
func CaptureWebcam(ctx context.Context, device string, fps int, frameCh chan<- []byte) error {
	return fmt.Errorf("webcam capture is not supported on this platform yet")
}

// CaptureWebcamPipeWireRaw is not supported on this platform.
func CaptureWebcamPipeWireRaw(ctx context.Context, fps int, frameCh chan<- RawFrame) error {
	return fmt.Errorf("PipeWire camera is not supported on this platform")
}
