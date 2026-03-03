package client

/*
#cgo pkg-config: vpx
#cgo LDFLAGS: -lm
#include <vpx/vpx_encoder.h>
#include <vpx/vpx_decoder.h>
#include <vpx/vp8cx.h>
#include <vpx/vp8dx.h>
#include <stdlib.h>
#include <string.h>

// ---- Encoder ----

typedef struct {
    vpx_codec_ctx_t     codec;
    vpx_image_t         raw;
    int                 width;
    int                 height;
    int                 fps;
    int                 inited;
    vpx_codec_pts_t     pts;
} vp8_encoder_t;

static vp8_encoder_t* vp8_encoder_new(int width, int height, int fps) {
    vp8_encoder_t *enc = calloc(1, sizeof(vp8_encoder_t));
    if (!enc) return NULL;

    vpx_codec_enc_cfg_t cfg;
    vpx_codec_enc_config_default(vpx_codec_vp8_cx(), &cfg, 0);
    cfg.g_w = width;
    cfg.g_h = height;
    cfg.g_timebase.num = 1;
    cfg.g_timebase.den = 1000;  // millisecond timebase for accurate timestamps
    // Dynamic bitrate based on resolution (kbps)
    int pixels = width * height;
    int bitrate;
    if (pixels >= 1920*1080) {
        bitrate = 8000;   // 8 Mbps for 1080p
    } else if (pixels >= 1280*720) {
        bitrate = 5000;   // 5 Mbps for 720p
    } else if (pixels >= 854*480) {
        bitrate = 3000;   // 3 Mbps for ~480p
    } else {
        bitrate = 1500;   // 1.5 Mbps for lower
    }
    cfg.rc_target_bitrate = bitrate;
    cfg.g_error_resilient = 1;
    cfg.g_threads = 4;
    cfg.g_pass = VPX_RC_ONE_PASS;
    cfg.rc_end_usage = VPX_VBR;     // VBR allows higher peaks for complex scenes
    cfg.kf_max_dist = fps * 3;      // keyframe every 3 seconds
    cfg.kf_min_dist = 0;
    cfg.g_lag_in_frames = 0;        // zero latency
    cfg.rc_undershoot_pct = 100;
    cfg.rc_overshoot_pct = 100;     // allow VBR to peak at 2x target for sharp frames
    cfg.rc_buf_initial_sz = 1000;
    cfg.rc_buf_optimal_sz = 1000;
    cfg.rc_buf_sz = 1500;

    if (vpx_codec_enc_init(&enc->codec, vpx_codec_vp8_cx(), &cfg, 0) != VPX_CODEC_OK) {
        free(enc);
        return NULL;
    }

    // CPUUSED: 0=best quality, 16=fastest. 3 is a good realtime balance.
    vpx_codec_control(&enc->codec, VP8E_SET_CPUUSED, 3);
    // Enable temporal noise reduction for cleaner webcam output
    vpx_codec_control(&enc->codec, VP8E_SET_NOISE_SENSITIVITY, 1);

    if (!vpx_img_alloc(&enc->raw, VPX_IMG_FMT_I420, width, height, 1)) {
        vpx_codec_destroy(&enc->codec);
        free(enc);
        return NULL;
    }

    enc->width = width;
    enc->height = height;
    enc->fps = fps;
    enc->inited = 1;
    return enc;
}

// Encode a YCbCr frame. If force_keyframe is non-zero, emit a keyframe.
// Returns pointer to internal buffer (valid until next encode).
static int vp8_encoder_encode(vp8_encoder_t *enc,
    unsigned char *y_plane, int y_stride,
    unsigned char *cb_plane, int cb_stride,
    unsigned char *cr_plane, int cr_stride,
    int force_keyframe,
    const unsigned char **out_buf, int *out_sz) {

    *out_buf = NULL;
    *out_sz = 0;

    int w = enc->width;
    int h = enc->height;

    // Copy Y plane
    for (int row = 0; row < h; row++) {
        memcpy(enc->raw.planes[0] + row * enc->raw.stride[0],
               y_plane + row * y_stride, w);
    }
    // Copy Cb plane
    for (int row = 0; row < h / 2; row++) {
        memcpy(enc->raw.planes[1] + row * enc->raw.stride[1],
               cb_plane + row * cb_stride, w / 2);
    }
    // Copy Cr plane
    for (int row = 0; row < h / 2; row++) {
        memcpy(enc->raw.planes[2] + row * enc->raw.stride[2],
               cr_plane + row * cr_stride, w / 2);
    }

    vpx_enc_frame_flags_t flags = 0;
    if (force_keyframe) flags |= VPX_EFLAG_FORCE_KF;

    int frame_dur = 1000 / enc->fps;  // frame duration in ms
    vpx_codec_err_t err = vpx_codec_encode(&enc->codec, &enc->raw,
        enc->pts, frame_dur, flags, VPX_DL_REALTIME);
    if (err != VPX_CODEC_OK) return -1;
    enc->pts += frame_dur;

    vpx_codec_iter_t iter = NULL;
    const vpx_codec_cx_pkt_t *pkt;
    while ((pkt = vpx_codec_get_cx_data(&enc->codec, &iter)) != NULL) {
        if (pkt->kind == VPX_CODEC_CX_FRAME_PKT) {
            *out_buf = (const unsigned char*)pkt->data.frame.buf;
            *out_sz = (int)pkt->data.frame.sz;
            return 0;
        }
    }
    return 0; // no frame produced (normal)
}

static void vp8_encoder_free(vp8_encoder_t *enc) {
    if (enc) {
        if (enc->inited) {
            vpx_img_free(&enc->raw);
            vpx_codec_destroy(&enc->codec);
        }
        free(enc);
    }
}

// ---- Decoder ----

typedef struct {
    vpx_codec_ctx_t codec;
    int             inited;
} vp8_decoder_t;

static vp8_decoder_t* vp8_decoder_new() {
    vp8_decoder_t *dec = calloc(1, sizeof(vp8_decoder_t));
    if (!dec) return NULL;

    if (vpx_codec_dec_init(&dec->codec, vpx_codec_vp8_dx(), NULL, 0) != VPX_CODEC_OK) {
        free(dec);
        return NULL;
    }

    dec->inited = 1;
    return dec;
}

// Decode VP8 data. Returns decoded image planes via out params.
// Plane pointers are valid until the next decode call.
static int vp8_decoder_decode(vp8_decoder_t *dec,
    const unsigned char *data, int data_sz,
    int *out_w, int *out_h,
    unsigned char **out_y, int *out_ystride,
    unsigned char **out_u, int *out_ustride,
    unsigned char **out_v, int *out_vstride) {

    *out_w = 0;
    *out_h = 0;

    vpx_codec_err_t err = vpx_codec_decode(&dec->codec, data, data_sz, NULL, 0);
    if (err != VPX_CODEC_OK) return -1;

    vpx_codec_iter_t iter = NULL;
    vpx_image_t *img = vpx_codec_get_frame(&dec->codec, &iter);
    if (!img) return 0; // no frame ready

    *out_w = img->d_w;
    *out_h = img->d_h;
    *out_y = img->planes[0];
    *out_ystride = img->stride[0];
    *out_u = img->planes[1];
    *out_ustride = img->stride[1];
    *out_v = img->planes[2];
    *out_vstride = img->stride[2];
    return 1; // frame available
}

static void vp8_decoder_free(vp8_decoder_t *dec) {
    if (dec) {
        if (dec->inited) {
            vpx_codec_destroy(&dec->codec);
        }
        free(dec);
    }
}
*/
import "C"
import (
	"fmt"
	"image"
	"unsafe"
)

// VP8Encoder wraps libvpx VP8 encoding via direct cgo.
type VP8Encoder struct {
	enc           *C.vp8_encoder_t
	width         int
	height        int
	forceKeyframe bool // set to true to force next frame as keyframe
}

// NewVP8Encoder creates a new VP8 encoder.
func NewVP8Encoder(width, height, fps int) (*VP8Encoder, error) {
	enc := C.vp8_encoder_new(C.int(width), C.int(height), C.int(fps))
	if enc == nil {
		return nil, fmt.Errorf("vpx encoder init failed")
	}
	return &VP8Encoder{enc: enc, width: width, height: height}, nil
}

// Encode encodes a raw YCbCr image to VP8. Returns nil if no frame produced.
func (e *VP8Encoder) Encode(ycbcr *image.YCbCr) ([]byte, error) {
	if e.enc == nil {
		return nil, fmt.Errorf("encoder not initialized")
	}

	kf := C.int(0)
	if e.forceKeyframe {
		kf = 1
		e.forceKeyframe = false
	}

	var outBuf *C.uchar
	var outSz C.int

	ret := C.vp8_encoder_encode(e.enc,
		(*C.uchar)(unsafe.Pointer(&ycbcr.Y[0])), C.int(ycbcr.YStride),
		(*C.uchar)(unsafe.Pointer(&ycbcr.Cb[0])), C.int(ycbcr.CStride),
		(*C.uchar)(unsafe.Pointer(&ycbcr.Cr[0])), C.int(ycbcr.CStride),
		kf,
		&outBuf, &outSz)
	if ret < 0 {
		return nil, fmt.Errorf("vpx encode failed")
	}
	if outSz == 0 || outBuf == nil {
		return nil, nil
	}

	// Copy from internal vpx buffer to Go-owned slice
	data := make([]byte, int(outSz))
	copy(data, (*[1 << 30]byte)(unsafe.Pointer(outBuf))[:outSz:outSz])
	return data, nil
}

// Close releases encoder resources.
func (e *VP8Encoder) Close() {
	if e.enc != nil {
		C.vp8_encoder_free(e.enc)
		e.enc = nil
	}
}

// VP8Decoder wraps libvpx VP8 decoding via direct cgo.
type VP8Decoder struct {
	dec *C.vp8_decoder_t
}

// NewVP8Decoder creates a new VP8 decoder.
func NewVP8Decoder() (*VP8Decoder, error) {
	dec := C.vp8_decoder_new()
	if dec == nil {
		return nil, fmt.Errorf("vpx decoder init failed")
	}
	return &VP8Decoder{dec: dec}, nil
}

// Decode decodes VP8 data and returns raw RGB pixels.
// Returns nil,0,0,nil if no frame is ready yet.
func (d *VP8Decoder) Decode(vp8Data []byte) (rgb []byte, width, height int, err error) {
	if d.dec == nil {
		return nil, 0, 0, fmt.Errorf("decoder not initialized")
	}
	if len(vp8Data) == 0 {
		return nil, 0, 0, nil
	}

	var outW, outH C.int
	var outY, outU, outV *C.uchar
	var outYStride, outUStride, outVStride C.int

	ret := C.vp8_decoder_decode(d.dec,
		(*C.uchar)(unsafe.Pointer(&vp8Data[0])), C.int(len(vp8Data)),
		&outW, &outH,
		&outY, &outYStride,
		&outU, &outUStride,
		&outV, &outVStride)

	if ret < 0 {
		return nil, 0, 0, fmt.Errorf("vpx decode failed")
	}
	if ret == 0 {
		return nil, 0, 0, nil // no frame ready
	}

	w := int(outW)
	h := int(outH)

	// Convert I420 planes to packed RGB
	yPlane := (*[1 << 30]byte)(unsafe.Pointer(outY))[:h*int(outYStride)]
	uPlane := (*[1 << 30]byte)(unsafe.Pointer(outU))[:(h/2)*int(outUStride)]
	vPlane := (*[1 << 30]byte)(unsafe.Pointer(outV))[:(h/2)*int(outVStride)]
	yStride := int(outYStride)
	uStride := int(outUStride)
	vStride := int(outVStride)

	rgb = yuv420ToRGB(yPlane, uPlane, vPlane, yStride, uStride, vStride, w, h)
	return rgb, w, h, nil
}

// DecodeSync feeds VP8 data through the decoder to keep its internal state
// in sync, but skips the expensive YUV→RGB conversion. Returns (width, height, error).
func (d *VP8Decoder) DecodeSync(vp8Data []byte) (int, int, error) {
	if d.dec == nil {
		return 0, 0, fmt.Errorf("decoder not initialized")
	}
	if len(vp8Data) == 0 {
		return 0, 0, nil
	}

	var outW, outH C.int
	var outY, outU, outV *C.uchar
	var outYStride, outUStride, outVStride C.int

	ret := C.vp8_decoder_decode(d.dec,
		(*C.uchar)(unsafe.Pointer(&vp8Data[0])), C.int(len(vp8Data)),
		&outW, &outH,
		&outY, &outYStride,
		&outU, &outUStride,
		&outV, &outVStride)

	if ret < 0 {
		return 0, 0, fmt.Errorf("vpx decode failed")
	}
	return int(outW), int(outH), nil
}

// Close releases decoder resources.
func (d *VP8Decoder) Close() {
	if d.dec != nil {
		C.vp8_decoder_free(d.dec)
		d.dec = nil
	}
}

// yuv420ToRGB converts I420 planes to packed RGB.
// Uses arithmetic right shift (>>16) instead of division (/65536) for speed.
func yuv420ToRGB(yPlane, uPlane, vPlane []byte, yStride, uStride, vStride, w, h int) []byte {
	rgb := make([]byte, w*h*3)
	idx := 0
	for y := 0; y < h; y++ {
		yRow := yPlane[y*yStride:]
		uRow := uPlane[(y/2)*uStride:]
		vRow := vPlane[(y/2)*vStride:]
		for x := 0; x < w; x++ {
			yy := int32(yRow[x])
			uu := int32(uRow[x/2]) - 128
			vv := int32(vRow[x/2]) - 128

			r := yy + (91881*vv)>>16
			g := yy - (22554*uu)>>16 - (46802*vv)>>16
			b := yy + (116130*uu)>>16

			rgb[idx] = clampByte(r)
			rgb[idx+1] = clampByte(g)
			rgb[idx+2] = clampByte(b)
			idx += 3
		}
	}
	return rgb
}

func clampByte(v int32) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
