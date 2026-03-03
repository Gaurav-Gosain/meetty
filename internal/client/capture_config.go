package client

// Capture configuration defaults. These are the target values passed to
// the capture backends; actual values depend on device capabilities.

const (
	// Camera capture
	CameraMaxWidth  = 1280
	CameraMaxHeight = 720
	CameraFPS       = 30
	CameraJPEGQuality = 85 // macOS BGRA→JPEG intermediate encoding

	// Screen capture
	ScreenMaxWidth = 1920
	ScreenFPS      = 15
)
