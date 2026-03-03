//go:build linux

package client

/*
#cgo pkg-config: libpipewire-0.3
#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>
#include <spa/param/video/type-info.h>
#include <spa/utils/result.h>
#include <spa/buffer/buffer.h>
#include <string.h>
#include <unistd.h>
#include <sys/mman.h>

// Forward declarations of Go callbacks
extern void goCameraFrameCallback(void *data, int width, int height, int stride, unsigned int format);
extern void goCameraStreamReady();
extern void goCameraStreamError(char *msg);

struct camera_capture {
	struct pw_main_loop *loop;
	struct pw_context *context;
	struct pw_core *core;
	struct pw_stream *stream;
	struct spa_hook stream_listener;
	struct spa_video_info format;
	int width;
	int height;
	int ready;
};

static struct camera_capture *global_cam_capture = NULL;

static void on_cam_stream_param_changed(void *userdata, uint32_t id, const struct spa_pod *param) {
	struct camera_capture *cap = userdata;
	if (param == NULL || id != SPA_PARAM_Format) return;

	if (spa_format_video_raw_parse(param, &cap->format.info.raw) < 0) return;

	cap->width = cap->format.info.raw.size.width;
	cap->height = cap->format.info.raw.size.height;

	int bpp = 4;
	if (cap->format.info.raw.format == SPA_VIDEO_FORMAT_RGB ||
	    cap->format.info.raw.format == SPA_VIDEO_FORMAT_BGR)
		bpp = 3;

	int stride = cap->width * bpp;
	int size = stride * cap->height;

	uint8_t params_buffer[1024];
	struct spa_pod_builder b = SPA_POD_BUILDER_INIT(params_buffer, sizeof(params_buffer));

	const struct spa_pod *bufparams = spa_pod_builder_add_object(&b,
		SPA_TYPE_OBJECT_ParamBuffers, SPA_PARAM_Buffers,
		SPA_PARAM_BUFFERS_buffers, SPA_POD_CHOICE_RANGE_Int(4, 1, 8),
		SPA_PARAM_BUFFERS_size, SPA_POD_Int(size),
		SPA_PARAM_BUFFERS_stride, SPA_POD_Int(stride),
		SPA_PARAM_BUFFERS_dataType, SPA_POD_CHOICE_FLAGS_Int(
			(1 << SPA_DATA_MemFd) | (1 << SPA_DATA_MemPtr) | (1 << SPA_DATA_DmaBuf)));

	pw_stream_update_params(cap->stream, &bufparams, 1);
}

static void on_cam_stream_state_changed(void *userdata, enum pw_stream_state old,
                                        enum pw_stream_state state, const char *error) {
	struct camera_capture *cap = userdata;

	if (state == PW_STREAM_STATE_STREAMING && !cap->ready) {
		cap->ready = 1;
		goCameraStreamReady();
	}

	if (state == PW_STREAM_STATE_ERROR) {
		goCameraStreamError(error ? (char*)error : (char*)"unknown error");
		pw_main_loop_quit(cap->loop);
	}
}

static void on_cam_stream_process(void *userdata) {
	struct camera_capture *cap = userdata;
	struct pw_buffer *b = pw_stream_dequeue_buffer(cap->stream);
	if (!b) return;

	struct spa_buffer *buf = b->buffer;
	if (buf->n_datas < 1) {
		pw_stream_queue_buffer(cap->stream, b);
		return;
	}

	void *data = NULL;
	void *mapped = NULL;
	size_t mapped_size = 0;
	struct spa_data *d = &buf->datas[0];

	if (d->type == SPA_DATA_DmaBuf || d->type == SPA_DATA_MemFd) {
		mapped_size = d->maxsize;
		mapped = mmap(NULL, mapped_size, PROT_READ, MAP_SHARED, d->fd, d->mapoffset);
		if (mapped != MAP_FAILED) {
			data = mapped;
		}
	} else if (d->data != NULL) {
		data = d->data;
	}

	if (data != NULL && cap->width > 0 && cap->height > 0) {
		int stride = d->chunk->stride;
		int bpp = 4;
		if (cap->format.info.raw.format == SPA_VIDEO_FORMAT_RGB ||
		    cap->format.info.raw.format == SPA_VIDEO_FORMAT_BGR)
			bpp = 3;
		if (stride == 0) stride = cap->width * bpp;

		goCameraFrameCallback(data, cap->width, cap->height, stride,
		                      cap->format.info.raw.format);
	}

	if (mapped != NULL && mapped != MAP_FAILED) {
		munmap(mapped, mapped_size);
	}

	pw_stream_queue_buffer(cap->stream, b);
}

static const struct pw_stream_events cam_stream_events = {
	PW_VERSION_STREAM_EVENTS,
	.state_changed = on_cam_stream_state_changed,
	.param_changed = on_cam_stream_param_changed,
	.process = on_cam_stream_process,
};

// pw_camera_capture_start connects directly to the PipeWire daemon (no portal)
// and streams from the default camera. This allows sharing the camera with
// browsers and other PipeWire clients.
static int pw_camera_capture_start() {
	pw_init(NULL, NULL);

	struct camera_capture *cap = calloc(1, sizeof(struct camera_capture));
	if (!cap) return -1;
	global_cam_capture = cap;

	cap->loop = pw_main_loop_new(NULL);
	if (!cap->loop) { free(cap); return -1; }

	cap->context = pw_context_new(pw_main_loop_get_loop(cap->loop), NULL, 0);
	if (!cap->context) {
		pw_main_loop_destroy(cap->loop);
		free(cap);
		return -1;
	}

	// Connect directly to PipeWire daemon — no portal, no permission dialog.
	// This works for native (non-Flatpak) apps and allows camera sharing.
	cap->core = pw_context_connect(cap->context, NULL, 0);
	if (!cap->core) {
		pw_context_destroy(cap->context);
		pw_main_loop_destroy(cap->loop);
		free(cap);
		return -2;
	}

	cap->stream = pw_stream_new(cap->core, "meetty-webrtc-camera",
		pw_properties_new(
			PW_KEY_MEDIA_TYPE, "Video",
			PW_KEY_MEDIA_CATEGORY, "Capture",
			PW_KEY_MEDIA_ROLE, "Camera",
			NULL));
	if (!cap->stream) {
		pw_core_disconnect(cap->core);
		pw_context_destroy(cap->context);
		pw_main_loop_destroy(cap->loop);
		free(cap);
		return -3;
	}

	pw_stream_add_listener(cap->stream, &cap->stream_listener, &cam_stream_events, cap);

	uint8_t buffer[2048];
	struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buffer, sizeof(buffer));
	const struct spa_pod *params[1];

	// Accept common RGBA/RGB pixel formats from camera
	params[0] = spa_pod_builder_add_object(&b,
		SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat,
		SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video),
		SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw),
		SPA_FORMAT_VIDEO_format, SPA_POD_CHOICE_ENUM_Id(11,
			SPA_VIDEO_FORMAT_BGRx,
			SPA_VIDEO_FORMAT_BGRx,
			SPA_VIDEO_FORMAT_RGBx,
			SPA_VIDEO_FORMAT_xRGB,
			SPA_VIDEO_FORMAT_xBGR,
			SPA_VIDEO_FORMAT_BGRA,
			SPA_VIDEO_FORMAT_RGBA,
			SPA_VIDEO_FORMAT_ARGB,
			SPA_VIDEO_FORMAT_ABGR,
			SPA_VIDEO_FORMAT_RGB,
			SPA_VIDEO_FORMAT_BGR),
		SPA_FORMAT_VIDEO_size, SPA_POD_CHOICE_RANGE_Rectangle(
			&SPA_RECTANGLE(1280, 720),
			&SPA_RECTANGLE(1, 1),
			&SPA_RECTANGLE(1920, 1080)),
		SPA_FORMAT_VIDEO_framerate, SPA_POD_CHOICE_RANGE_Fraction(
			&SPA_FRACTION(30, 1),
			&SPA_FRACTION(0, 1),
			&SPA_FRACTION(60, 1)));

	// PW_ID_ANY with AUTOCONNECT — PipeWire picks the default camera.
	// The media.class filter (Video/Capture + Camera role) ensures it
	// connects to a camera node, not a screen capture node.
	if (pw_stream_connect(cap->stream, PW_DIRECTION_INPUT, PW_ID_ANY,
			PW_STREAM_FLAG_AUTOCONNECT | PW_STREAM_FLAG_MAP_BUFFERS,
			params, 1) < 0) {
		pw_stream_destroy(cap->stream);
		pw_core_disconnect(cap->core);
		pw_context_destroy(cap->context);
		pw_main_loop_destroy(cap->loop);
		free(cap);
		return -4;
	}

	pw_main_loop_run(cap->loop);

	pw_stream_destroy(cap->stream);
	pw_core_disconnect(cap->core);
	pw_context_destroy(cap->context);
	pw_main_loop_destroy(cap->loop);
	free(cap);
	global_cam_capture = NULL;

	return 0;
}

static void pw_camera_capture_stop() {
	if (global_cam_capture && global_cam_capture->loop) {
		pw_main_loop_quit(global_cam_capture->loop);
	}
}
*/
import "C"

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

var (
	globalCamMu    sync.Mutex
	globalCamCh    chan RawFrame
	globalCamReady chan struct{}
	globalCamErr   chan string
)

//export goCameraFrameCallback
func goCameraFrameCallback(data unsafe.Pointer, width, height, stride C.int, format C.uint) {
	globalCamMu.Lock()
	ch := globalCamCh
	if ch == nil {
		globalCamMu.Unlock()
		return
	}

	h := int(height)
	s := int(stride)
	pixels := C.GoBytes(data, C.int(h*s))

	frame := RawFrame{
		Pixels: pixels,
		Width:  int(width),
		Height: h,
		Stride: s,
		Format: uint32(format),
	}

	select {
	case ch <- frame:
	default:
	}
	globalCamMu.Unlock()
}

//export goCameraStreamReady
func goCameraStreamReady() {
	globalCamMu.Lock()
	ch := globalCamReady
	globalCamMu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

//export goCameraStreamError
func goCameraStreamError(msg *C.char) {
	globalCamMu.Lock()
	ch := globalCamErr
	globalCamMu.Unlock()
	if ch != nil {
		select {
		case ch <- C.GoString(msg):
		default:
		}
	}
}

// CaptureWebcamPipeWireRaw captures from the camera by connecting directly to
// the PipeWire daemon. This allows sharing the camera device with browsers
// and other PipeWire clients (no exclusive V4L2 lock). Sends raw RawFrame
// structs directly, bypassing JPEG encoding for zero-loss VP8 pipeline.
func CaptureWebcamPipeWireRaw(ctx context.Context, fps int, frameCh chan<- RawFrame) error {
	if fps <= 0 {
		fps = CameraFPS
	}

	rawCh := make(chan RawFrame, 2)
	readyCh := make(chan struct{}, 1)
	errCh := make(chan string, 1)

	globalCamMu.Lock()
	globalCamCh = rawCh
	globalCamReady = readyCh
	globalCamErr = errCh
	globalCamMu.Unlock()

	pwDone := make(chan int, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ret := C.pw_camera_capture_start()
		pwDone <- int(ret)
	}()

	defer func() {
		C.pw_camera_capture_stop()
		select {
		case <-pwDone:
		case <-time.After(3 * time.Second):
		}
		globalCamMu.Lock()
		globalCamCh = nil
		globalCamReady = nil
		globalCamErr = nil
		globalCamMu.Unlock()
	}()

	// Wait for stream to start
	select {
	case <-readyCh:
		log.Printf("PipeWire camera stream ready")
	case <-ctx.Done():
		return ctx.Err()
	case errMsg := <-errCh:
		return fmt.Errorf("PipeWire camera error: %s", errMsg)
	case ret := <-pwDone:
		return fmt.Errorf("PipeWire camera failed (code %d)", ret)
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for PipeWire camera stream")
	}

	interval := time.Second / time.Duration(fps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastFrame RawFrame

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pwDone:
			return nil
		case frame := <-rawCh:
			lastFrame = frame
		case <-ticker.C:
			if lastFrame.Pixels == nil {
				continue
			}
			select {
			case frameCh <- lastFrame:
			default:
			}
		}
	}
}

