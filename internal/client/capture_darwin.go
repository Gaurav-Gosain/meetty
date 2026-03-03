//go:build darwin

package client

/*
#cgo darwin CFLAGS: -x objective-c -fobjc-arc
#cgo darwin LDFLAGS: -framework AVFoundation -framework CoreMedia -framework CoreVideo -framework Foundation

#import <AVFoundation/AVFoundation.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <Foundation/Foundation.h>
#include <string.h>

// Forward declaration of Go callback
extern void goWebcamFrameCallback(void *data, int width, int height, int bytesPerRow);

// Delegate that receives camera frames
@interface MeettyWebcamDelegate : NSObject<AVCaptureVideoDataOutputSampleBufferDelegate>
@end

@implementation MeettyWebcamDelegate
- (void)captureOutput:(AVCaptureOutput *)output
 didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
        fromConnection:(AVCaptureConnection *)connection
{
	CVImageBufferRef imageBuffer = CMSampleBufferGetImageBuffer(sampleBuffer);
	if (!imageBuffer) return;

	CVPixelBufferLockBaseAddress(imageBuffer, kCVPixelBufferLock_ReadOnly);

	void *baseAddress = CVPixelBufferGetBaseAddress(imageBuffer);
	size_t width = CVPixelBufferGetWidth(imageBuffer);
	size_t height = CVPixelBufferGetHeight(imageBuffer);
	size_t bytesPerRow = CVPixelBufferGetBytesPerRow(imageBuffer);

	if (baseAddress) {
		goWebcamFrameCallback(baseAddress, (int)width, (int)height, (int)bytesPerRow);
	}

	CVPixelBufferUnlockBaseAddress(imageBuffer, kCVPixelBufferLock_ReadOnly);
}
@end

static AVCaptureSession *captureSession = nil;
static MeettyWebcamDelegate *frameDelegate = nil;
static dispatch_queue_t captureQueue = nil;

// meetty_device_count returns the number of available video devices.
int meetty_device_count(void) {
	AVCaptureDeviceDiscoverySession *discovery = [AVCaptureDeviceDiscoverySession
		discoverySessionWithDeviceTypes:@[AVCaptureDeviceTypeBuiltInWideAngleCamera, AVCaptureDeviceTypeExternalUnknown]
		mediaType:AVMediaTypeVideo
		position:AVCaptureDevicePositionUnspecified];
	return (int)discovery.devices.count;
}

// meetty_device_name copies the name of device at index into buf.
int meetty_device_name(int index, char *buf, int bufLen) {
	AVCaptureDeviceDiscoverySession *discovery = [AVCaptureDeviceDiscoverySession
		discoverySessionWithDeviceTypes:@[AVCaptureDeviceTypeBuiltInWideAngleCamera, AVCaptureDeviceTypeExternalUnknown]
		mediaType:AVMediaTypeVideo
		position:AVCaptureDevicePositionUnspecified];
	NSArray<AVCaptureDevice *> *devices = discovery.devices;
	if (index < 0 || index >= (int)devices.count) return -1;
	const char *name = [devices[index].localizedName UTF8String];
	strlcpy(buf, name, bufLen);
	return 0;
}

// meetty_start_capture starts capturing from the device at the given index.
int meetty_start_capture(int deviceIndex, int width, int height) {
	AVCaptureDeviceDiscoverySession *discovery = [AVCaptureDeviceDiscoverySession
		discoverySessionWithDeviceTypes:@[AVCaptureDeviceTypeBuiltInWideAngleCamera, AVCaptureDeviceTypeExternalUnknown]
		mediaType:AVMediaTypeVideo
		position:AVCaptureDevicePositionUnspecified];
	NSArray<AVCaptureDevice *> *devices = discovery.devices;
	if (deviceIndex < 0 || deviceIndex >= (int)devices.count) return -1;

	AVCaptureDevice *device = devices[deviceIndex];

	captureSession = [[AVCaptureSession alloc] init];

	if (width <= 352 && height <= 288) {
		captureSession.sessionPreset = AVCaptureSessionPreset352x288;
	} else if (width <= 640 && height <= 480) {
		captureSession.sessionPreset = AVCaptureSessionPreset640x480;
	} else {
		captureSession.sessionPreset = AVCaptureSessionPreset1280x720;
	}

	NSError *error = nil;
	AVCaptureDeviceInput *input = [AVCaptureDeviceInput deviceInputWithDevice:device error:&error];
	if (!input || error) return -2;

	if ([captureSession canAddInput:input]) {
		[captureSession addInput:input];
	} else {
		captureSession = nil;
		return -3;
	}

	AVCaptureVideoDataOutput *output = [[AVCaptureVideoDataOutput alloc] init];
	output.alwaysDiscardsLateVideoFrames = YES;
	output.videoSettings = @{
		(NSString *)kCVPixelBufferPixelFormatTypeKey: @(kCVPixelFormatType_32BGRA)
	};

	frameDelegate = [[MeettyWebcamDelegate alloc] init];
	captureQueue = dispatch_queue_create("com.meetty.webcam", DISPATCH_QUEUE_SERIAL);
	[output setSampleBufferDelegate:frameDelegate queue:captureQueue];

	if ([captureSession canAddOutput:output]) {
		[captureSession addOutput:output];
	} else {
		captureSession = nil;
		frameDelegate = nil;
		return -4;
	}

	[captureSession startRunning];
	return 0;
}

// meetty_stop_capture stops the capture session.
void meetty_stop_capture(void) {
	if (captureSession) {
		[captureSession stopRunning];
		captureSession = nil;
	}
	frameDelegate = nil;
	captureQueue = nil;
}
*/
import "C"

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"strconv"
	"sync"
	"time"
	"unsafe"
)

// CameraDevice represents an available webcam.
type CameraDevice struct {
	Path string
	Name string
}

var (
	webcamMu    sync.Mutex
	webcamCh    chan []byte
	webcamReady bool
)

//export goWebcamFrameCallback
func goWebcamFrameCallback(data unsafe.Pointer, width, height, bytesPerRow C.int) {
	webcamMu.Lock()
	ch := webcamCh
	if ch == nil || !webcamReady {
		webcamMu.Unlock()
		return
	}

	w := int(width)
	h := int(height)
	bpr := int(bytesPerRow)

	// Copy BGRA pixels (the buffer is owned by AVFoundation and will be reused)
	totalBytes := h * bpr
	pixels := C.GoBytes(data, C.int(totalBytes))

	webcamMu.Unlock()

	// Convert BGRA to JPEG
	jpegData := bgraToJPEG(pixels, w, h, bpr)
	if jpegData == nil {
		return
	}

	select {
	case ch <- jpegData:
	default:
	}
}

// bgraToJPEG converts BGRA pixel data to JPEG.
func bgraToJPEG(bgra []byte, w, h, stride int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		srcRow := bgra[y*stride:]
		dstRow := img.Pix[y*img.Stride:]
		for x := range w {
			s := x * 4
			d := x * 4
			dstRow[d+0] = srcRow[s+2] // R from BGRA
			dstRow[d+1] = srcRow[s+1] // G
			dstRow[d+2] = srcRow[s+0] // B
			dstRow[d+3] = 255         // A
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: CameraJPEGQuality}); err != nil {
		return nil
	}
	return buf.Bytes()
}

// ListCameras returns available video capture devices on macOS.
func ListCameras() []CameraDevice {
	count := int(C.meetty_device_count())
	var devices []CameraDevice
	for i := range count {
		var nameBuf [256]C.char
		if C.meetty_device_name(C.int(i), &nameBuf[0], 256) != 0 {
			continue
		}
		name := C.GoString(&nameBuf[0])
		devices = append(devices, CameraDevice{
			Path: strconv.Itoa(i),
			Name: name,
		})
	}
	return devices
}

// CaptureWebcam captures frames from a webcam on macOS using AVFoundation.
// The device parameter is the device index as a string (e.g., "0").
// Sends JPEG frames on frameCh.
func CaptureWebcam(ctx context.Context, device string, fps int, frameCh chan<- []byte) error {
	if device == "" {
		device = "0"
	}
	if fps <= 0 {
		fps = CameraFPS
	}

	deviceIndex, err := strconv.Atoi(device)
	if err != nil {
		return fmt.Errorf("invalid device index %q: %w", device, err)
	}

	internalCh := make(chan []byte, 1)

	webcamMu.Lock()
	webcamCh = internalCh
	webcamReady = false
	webcamMu.Unlock()

	defer func() {
		webcamMu.Lock()
		webcamCh = nil
		webcamReady = false
		webcamMu.Unlock()
	}()

	ret := C.meetty_start_capture(C.int(deviceIndex), CameraMaxWidth, CameraMaxHeight)
	if ret != 0 {
		return fmt.Errorf("failed to start capture (code %d)", int(ret))
	}
	defer C.meetty_stop_capture()

	webcamMu.Lock()
	webcamReady = true
	webcamMu.Unlock()

	interval := time.Second / time.Duration(fps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var latestFrame []byte

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame := <-internalCh:
			latestFrame = frame
		case <-ticker.C:
			if latestFrame != nil {
				select {
				case frameCh <- latestFrame:
				default:
				}
				latestFrame = nil
			}
		}
	}
}

// CaptureWebcamPipeWireRaw is not supported on macOS (PipeWire is Linux-only).
func CaptureWebcamPipeWireRaw(ctx context.Context, fps int, frameCh chan<- RawFrame) error {
	return fmt.Errorf("PipeWire camera is not supported on macOS")
}
