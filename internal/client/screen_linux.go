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
extern void goScreenFrameCallback(void *data, int width, int height, int stride, unsigned int format);
extern void goScreenStreamReady();
extern void goScreenStreamError(char *msg);

struct screen_capture {
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

static struct screen_capture *global_capture = NULL;

static void on_stream_param_changed(void *userdata, uint32_t id, const struct spa_pod *param) {
	struct screen_capture *cap = userdata;
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

static void on_stream_state_changed(void *userdata, enum pw_stream_state old,
                                    enum pw_stream_state state, const char *error) {
	struct screen_capture *cap = userdata;

	if (state == PW_STREAM_STATE_STREAMING && !cap->ready) {
		cap->ready = 1;
		goScreenStreamReady();
	}

	if (state == PW_STREAM_STATE_ERROR) {
		goScreenStreamError(error ? (char*)error : (char*)"unknown error");
		pw_main_loop_quit(cap->loop);
	}
}

static void on_stream_process(void *userdata) {
	struct screen_capture *cap = userdata;
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

		goScreenFrameCallback(data, cap->width, cap->height, stride,
		                      cap->format.info.raw.format);
	}

	if (mapped != NULL && mapped != MAP_FAILED) {
		munmap(mapped, mapped_size);
	}

	pw_stream_queue_buffer(cap->stream, b);
}

static const struct pw_stream_events stream_events = {
	PW_VERSION_STREAM_EVENTS,
	.state_changed = on_stream_state_changed,
	.param_changed = on_stream_param_changed,
	.process = on_stream_process,
};

static int pw_inited = 0;

static int pw_screen_capture_start(int fd, uint32_t node_id) {
	if (!pw_inited) { pw_init(NULL, NULL); pw_inited = 1; }

	struct screen_capture *cap = calloc(1, sizeof(struct screen_capture));
	if (!cap) return -1;
	global_capture = cap;

	cap->loop = pw_main_loop_new(NULL);
	if (!cap->loop) { free(cap); return -1; }

	cap->context = pw_context_new(pw_main_loop_get_loop(cap->loop), NULL, 0);
	if (!cap->context) {
		pw_main_loop_destroy(cap->loop);
		free(cap);
		return -1;
	}

	cap->core = pw_context_connect_fd(cap->context, dup(fd), NULL, 0);
	if (!cap->core) {
		pw_context_destroy(cap->context);
		pw_main_loop_destroy(cap->loop);
		free(cap);
		return -2;
	}

	cap->stream = pw_stream_new(cap->core, "meetty-webrtc-screen-capture",
		pw_properties_new(
			PW_KEY_MEDIA_TYPE, "Video",
			PW_KEY_MEDIA_CATEGORY, "Capture",
			PW_KEY_MEDIA_ROLE, "Screen",
			NULL));
	if (!cap->stream) {
		pw_core_disconnect(cap->core);
		pw_context_destroy(cap->context);
		pw_main_loop_destroy(cap->loop);
		free(cap);
		return -3;
	}

	pw_stream_add_listener(cap->stream, &cap->stream_listener, &stream_events, cap);

	uint8_t buffer[2048];
	struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buffer, sizeof(buffer));
	const struct spa_pod *params[1];

	// Accept all common RGBA/RGB pixel formats
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
			&SPA_RECTANGLE(1920, 1080),
			&SPA_RECTANGLE(1, 1),
			&SPA_RECTANGLE(8192, 8192)),
		SPA_FORMAT_VIDEO_framerate, SPA_POD_CHOICE_RANGE_Fraction(
			&SPA_FRACTION(5, 1),
			&SPA_FRACTION(0, 1),
			&SPA_FRACTION(60, 1)));

	if (pw_stream_connect(cap->stream, PW_DIRECTION_INPUT, node_id,
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
	global_capture = NULL;

	return 0;
}

static void pw_screen_capture_stop() {
	if (global_capture && global_capture->loop) {
		pw_main_loop_quit(global_capture->loop);
	}
}
*/
import "C"

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/godbus/dbus/v5"
)

const (
	portalDest      = "org.freedesktop.portal.Desktop"
	portalPath      = "/org/freedesktop/portal/desktop"
	screenCastIface = "org.freedesktop.portal.ScreenCast"
)

// RawFrame holds raw pixel data from PipeWire screen capture.
type RawFrame struct {
	Pixels []byte
	Width  int
	Height int
	Stride int
	Format uint32
}

var (
	globalScreenMu    sync.Mutex
	globalScreenCh    chan RawFrame
	globalScreenReady chan struct{}
	globalScreenErr   chan string
)

//export goScreenFrameCallback
func goScreenFrameCallback(data unsafe.Pointer, width, height, stride C.int, format C.uint) {
	globalScreenMu.Lock()
	ch := globalScreenCh
	if ch == nil {
		globalScreenMu.Unlock()
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
	globalScreenMu.Unlock()
}

//export goScreenStreamReady
func goScreenStreamReady() {
	globalScreenMu.Lock()
	ch := globalScreenReady
	globalScreenMu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

//export goScreenStreamError
func goScreenStreamError(msg *C.char) {
	globalScreenMu.Lock()
	ch := globalScreenErr
	globalScreenMu.Unlock()
	if ch != nil {
		select {
		case ch <- C.GoString(msg):
		default:
		}
	}
}

// CaptureScreen captures the screen using xdg-desktop-portal (ScreenCast) and
// PipeWire for continuous frame streaming. The compositor shows a native screen
// picker dialog. Frames are encoded as PNG and sent on frameCh.
func CaptureScreen(ctx context.Context, fps int, frameCh chan<- []byte) error {
	if fps <= 0 {
		fps = ScreenFPS
	}

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("connect session bus: %w", err)
	}
	defer conn.Close()

	nodeID, pwFD, cleanup, err := setupPortalScreencast(conn)
	if err != nil {
		return fmt.Errorf("portal screencast: %w", err)
	}
	defer cleanup()

	rawCh := make(chan RawFrame, 2)
	readyCh := make(chan struct{}, 1)
	errCh := make(chan string, 1)

	globalScreenMu.Lock()
	globalScreenCh = rawCh
	globalScreenReady = readyCh
	globalScreenErr = errCh
	globalScreenMu.Unlock()

	pwDone := make(chan int, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ret := C.pw_screen_capture_start(C.int(pwFD), C.uint32_t(nodeID))
		pwDone <- int(ret)
	}()

	defer func() {
		C.pw_screen_capture_stop()
		<-pwDone
		globalScreenMu.Lock()
		globalScreenCh = nil
		globalScreenReady = nil
		globalScreenErr = nil
		globalScreenMu.Unlock()
	}()

	// Wait for stream to start
	select {
	case <-readyCh:
	case <-ctx.Done():
		return ctx.Err()
	case errMsg := <-errCh:
		return fmt.Errorf("PipeWire stream error: %s", errMsg)
	case ret := <-pwDone:
		return fmt.Errorf("PipeWire capture failed (code %d)", ret)
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for PipeWire stream")
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
			pngData := encodeRawToPNG(lastFrame)
			if pngData != nil {
				select {
				case frameCh <- pngData:
				default:
				}
			}
		}
	}
}

// PipeWire SPA video format constants (from spa/param/video/raw.h enum spa_video_format).
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

// CaptureScreenRaw captures the screen and sends raw RawFrame structs directly,
// bypassing PNG encoding. This is used by the WebRTC pipeline to go straight to
// VP8 without a PNG encode/decode roundtrip.
func CaptureScreenRaw(ctx context.Context, fps int, frameCh chan<- RawFrame) error {
	if fps <= 0 {
		fps = ScreenFPS
	}

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("connect session bus: %w", err)
	}
	defer conn.Close()

	nodeID, pwFD, cleanup, err := setupPortalScreencast(conn)
	if err != nil {
		return fmt.Errorf("portal screencast: %w", err)
	}
	defer cleanup()

	rawCh := make(chan RawFrame, 2)
	readyCh := make(chan struct{}, 1)
	errCh := make(chan string, 1)

	globalScreenMu.Lock()
	globalScreenCh = rawCh
	globalScreenReady = readyCh
	globalScreenErr = errCh
	globalScreenMu.Unlock()

	pwDone := make(chan int, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ret := C.pw_screen_capture_start(C.int(pwFD), C.uint32_t(nodeID))
		pwDone <- int(ret)
	}()

	// Ensure PipeWire is fully stopped and C goroutine finished before
	// clearing globals, so a subsequent screen share doesn't race.
	defer func() {
		C.pw_screen_capture_stop()
		<-pwDone
		globalScreenMu.Lock()
		globalScreenCh = nil
		globalScreenReady = nil
		globalScreenErr = nil
		globalScreenMu.Unlock()
	}()

	select {
	case <-readyCh:
	case <-ctx.Done():
		return ctx.Err()
	case errMsg := <-errCh:
		return fmt.Errorf("PipeWire stream error: %s", errMsg)
	case ret := <-pwDone:
		return fmt.Errorf("PipeWire capture failed (code %d)", ret)
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for PipeWire stream")
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

// RawFrameToRGB converts a raw PipeWire frame to packed RGB bytes, with optional
// downscaling to screenMaxWidth. This avoids the PNG encode/decode roundtrip.
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

// RawFrameToYCbCr420 converts a raw PipeWire frame directly to YCbCr 4:2:0
// for VP8 encoding, with optional downscaling. Avoids PNG and intermediate RGB.
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

// encodeRawToPNG converts raw PipeWire pixels to PNG (used by legacy CaptureScreen).
func encodeRawToPNG(frame RawFrame) []byte {
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

	img := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	for dy := range dstH {
		sy := dy * srcH / dstH
		for dx := range dstW {
			sx := dx * srcW / dstW
			s := sy*frame.Stride + sx*bpp
			d := dy*img.Stride + dx*4

			switch frame.Format {
			case spaVideoFormatBGRx, spaVideoFormatBGRA:
				img.Pix[d+0] = frame.Pixels[s+2]
				img.Pix[d+1] = frame.Pixels[s+1]
				img.Pix[d+2] = frame.Pixels[s+0]
			case spaVideoFormatRGBx, spaVideoFormatRGBA:
				img.Pix[d+0] = frame.Pixels[s+0]
				img.Pix[d+1] = frame.Pixels[s+1]
				img.Pix[d+2] = frame.Pixels[s+2]
			case spaVideoFormatXRGB, spaVideoFormatARGB:
				img.Pix[d+0] = frame.Pixels[s+1]
				img.Pix[d+1] = frame.Pixels[s+2]
				img.Pix[d+2] = frame.Pixels[s+3]
			case spaVideoFormatXBGR, spaVideoFormatABGR:
				img.Pix[d+0] = frame.Pixels[s+3]
				img.Pix[d+1] = frame.Pixels[s+2]
				img.Pix[d+2] = frame.Pixels[s+1]
			case spaVideoFormatRGB:
				img.Pix[d+0] = frame.Pixels[s+0]
				img.Pix[d+1] = frame.Pixels[s+1]
				img.Pix[d+2] = frame.Pixels[s+2]
			case spaVideoFormatBGR:
				img.Pix[d+0] = frame.Pixels[s+2]
				img.Pix[d+1] = frame.Pixels[s+1]
				img.Pix[d+2] = frame.Pixels[s+0]
			default:
				return nil
			}
			img.Pix[d+3] = 255
		}
	}

	var buf bytes.Buffer
	enc := &png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

func setupPortalScreencast(conn *dbus.Conn) (nodeID uint32, pwFD int, cleanup func(), err error) {
	portal := conn.Object(portalDest, portalPath)
	sender := portalSenderName(conn)
	ts := time.Now().UnixNano() % 1000000

	// 1. CreateSession
	createToken := fmt.Sprintf("meetty_c%d", ts)
	sessionToken := fmt.Sprintf("meetty_s%d", ts)
	createReqPath := dbus.ObjectPath(fmt.Sprintf(
		"/org/freedesktop/portal/desktop/request/%s/%s", sender, createToken))

	resp, err := portalRequest(conn, portal, createReqPath, "CreateSession",
		map[string]dbus.Variant{
			"handle_token":         dbus.MakeVariant(createToken),
			"session_handle_token": dbus.MakeVariant(sessionToken),
		})
	if err != nil {
		return 0, -1, nil, fmt.Errorf("CreateSession: %w", err)
	}

	sessionHandleVar, ok := resp["session_handle"]
	if !ok {
		return 0, -1, nil, fmt.Errorf("no session_handle in response")
	}
	sessionPath := dbus.ObjectPath(sessionHandleVar.Value().(string))

	cleanup = func() {
		conn.Object(portalDest, sessionPath).Call("org.freedesktop.portal.Session.Close", 0)
	}

	// 2. SelectSources (compositor shows screen picker)
	selectToken := fmt.Sprintf("meetty_sel%d", ts)
	selectReqPath := dbus.ObjectPath(fmt.Sprintf(
		"/org/freedesktop/portal/desktop/request/%s/%s", sender, selectToken))

	_, err = portalRequest(conn, portal, selectReqPath, "SelectSources",
		sessionPath,
		map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(selectToken),
			"types":        dbus.MakeVariant(uint32(1 | 2)), // MONITOR | WINDOW
			"cursor_mode":  dbus.MakeVariant(uint32(2)),     // Embedded
		})
	if err != nil {
		cleanup()
		return 0, -1, nil, fmt.Errorf("SelectSources: %w", err)
	}

	// 3. Start (triggers user dialog, returns PipeWire node ID)
	startToken := fmt.Sprintf("meetty_st%d", ts)
	startReqPath := dbus.ObjectPath(fmt.Sprintf(
		"/org/freedesktop/portal/desktop/request/%s/%s", sender, startToken))

	startResp, err := portalRequest(conn, portal, startReqPath, "Start",
		sessionPath,
		"", // parent_window
		map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(startToken),
		})
	if err != nil {
		cleanup()
		return 0, -1, nil, fmt.Errorf("start: %w", err)
	}

	streamsVar, ok := startResp["streams"]
	if !ok {
		cleanup()
		return 0, -1, nil, fmt.Errorf("no streams in Start response")
	}

	nodeID, err = parseStreamsNodeID(streamsVar)
	if err != nil {
		cleanup()
		return 0, -1, nil, err
	}

	// 4. OpenPipeWireRemote
	var fd dbus.UnixFD
	err = portal.Call(screenCastIface+".OpenPipeWireRemote", 0,
		sessionPath,
		map[string]dbus.Variant{}).Store(&fd)
	if err != nil {
		cleanup()
		return 0, -1, nil, fmt.Errorf("OpenPipeWireRemote: %w", err)
	}

	return nodeID, int(fd), cleanup, nil
}

func parseStreamsNodeID(v dbus.Variant) (uint32, error) {
	val := v.Value()

	switch streams := val.(type) {
	case [][]any:
		if len(streams) == 0 {
			return 0, fmt.Errorf("no streams available")
		}
		nodeID, ok := streams[0][0].(uint32)
		if !ok {
			return 0, fmt.Errorf("unexpected node_id type")
		}
		return nodeID, nil
	case []any:
		if len(streams) == 0 {
			return 0, fmt.Errorf("no streams available")
		}
		switch s := streams[0].(type) {
		case []any:
			nodeID, ok := s[0].(uint32)
			if !ok {
				return 0, fmt.Errorf("unexpected node_id type: %T", s[0])
			}
			return nodeID, nil
		default:
			return 0, fmt.Errorf("unexpected stream element type: %T", s)
		}
	default:
		return 0, fmt.Errorf("unexpected streams type: %T", val)
	}
}

func portalRequest(conn *dbus.Conn, portal dbus.BusObject,
	reqPath dbus.ObjectPath, method string, args ...any,
) (map[string]dbus.Variant, error) {
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(reqPath),
		dbus.WithMatchInterface("org.freedesktop.portal.Request"),
		dbus.WithMatchMember("Response"),
	); err != nil {
		return nil, err
	}

	sigCh := make(chan *dbus.Signal, 1)
	conn.Signal(sigCh)
	defer func() {
		conn.RemoveSignal(sigCh)
		conn.RemoveMatchSignal(
			dbus.WithMatchObjectPath(reqPath),
			dbus.WithMatchInterface("org.freedesktop.portal.Request"),
			dbus.WithMatchMember("Response"),
		)
	}()

	call := portal.Call(screenCastIface+"."+method, 0, args...)
	if call.Err != nil {
		return nil, call.Err
	}

	select {
	case sig := <-sigCh:
		if len(sig.Body) < 2 {
			return nil, fmt.Errorf("invalid response signal")
		}
		status, ok := sig.Body[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("unexpected status type: %T", sig.Body[0])
		}
		if status != 0 {
			if status == 1 {
				return nil, fmt.Errorf("user cancelled screen selection")
			}
			return nil, fmt.Errorf("portal request failed (status %d)", status)
		}
		results, ok := sig.Body[1].(map[string]dbus.Variant)
		if !ok {
			return nil, fmt.Errorf("unexpected results type: %T", sig.Body[1])
		}
		return results, nil
	case <-time.After(120 * time.Second):
		return nil, fmt.Errorf("portal request timed out (120s)")
	}
}

func portalSenderName(conn *dbus.Conn) string {
	name := conn.Names()[0]
	name = strings.ReplaceAll(name, ".", "_")
	name = strings.ReplaceAll(name, ":", "_")
	return name
}
