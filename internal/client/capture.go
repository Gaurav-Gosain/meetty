package client

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

// SampleWriter writes media samples. Implemented by both
// webrtc.TrackLocalStaticSample and lksdk.LocalSampleTrack.
type SampleWriter interface {
	WriteSample(media.Sample) error
}

// LocalFrameCallback is called with each decoded frame's RGB data for local preview.
type LocalFrameCallback func(rgb []byte, width, height int)

// EncoderCallback is called when a new encoder is created, allowing callers
// to keep a reference for PLI-triggered keyframe forcing.
type EncoderCallback func(enc *VP8Encoder)

// StartCapturePipeline captures webcam frames, encodes to VP8, and writes to the track.
// If onLocalFrame is non-nil, it's called with RGB data for local self-view.
// If onEncoder is non-nil, it's called when the encoder is created/recreated.
func StartCapturePipeline(ctx context.Context, device string, fps int, writer SampleWriter, onLocalFrame LocalFrameCallback, onEncoder EncoderCallback) error {
	frameCh := make(chan []byte, 2)

	go func() {
		if err := CaptureWebcam(ctx, device, fps, frameCh); err != nil {
			log.Printf("webcam capture error: %v", err)
		}
	}()

	var encoder *VP8Encoder
	interval := time.Second / time.Duration(fps)

	defer func() {
		if encoder != nil {
			encoder.Close()
		}
		if onEncoder != nil {
			onEncoder(nil)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case jpegData := <-frameCh:
			img, err := jpeg.Decode(bytes.NewReader(jpegData))
			if err != nil {
				continue
			}

			bounds := img.Bounds()
			w, h := bounds.Dx(), bounds.Dy()

			// Send local preview (works with any image type/subsampling)
			if onLocalFrame != nil {
				rgb := imageToRGB(img, w, h)
				onLocalFrame(rgb, w, h)
			}

			// Convert to 4:2:0 for VP8 encoder
			ycbcr := ImageToYCbCr420(img)

			if encoder == nil || encoder.width != w || encoder.height != h {
				if encoder != nil {
					encoder.Close()
				}
				encoder, err = NewVP8Encoder(w, h, fps)
				if err != nil {
					log.Printf("VP8 encoder init failed: %v", err)
					continue
				}
				// Force first frame as keyframe
				encoder.forceKeyframe = true
				if onEncoder != nil {
					onEncoder(encoder)
				}
			}

			vp8Data, err := encoder.Encode(ycbcr)
			if err != nil || vp8Data == nil {
				continue
			}

			if err := writer.WriteSample(media.Sample{
				Data:     vp8Data,
				Duration: interval,
			}); err != nil {
				log.Printf("write video sample: %v", err)
			}
		}
	}
}

// imageToRGB converts any image to packed RGB bytes using Go's standard color model.
func imageToRGB(img image.Image, w, h int) []byte {
	rgb := make([]byte, w*h*3)
	bounds := img.Bounds()
	idx := 0

	// Fast path for YCbCr (most common from JPEG)
	if ycbcr, ok := img.(*image.YCbCr); ok {
		for y := range h {
			for x := range w {
				r, g, b := color.YCbCrToRGB(
					ycbcr.Y[ycbcr.YOffset(x, y)],
					ycbcr.Cb[ycbcr.COffset(x, y)],
					ycbcr.Cr[ycbcr.COffset(x, y)],
				)
				rgb[idx] = r
				rgb[idx+1] = g
				rgb[idx+2] = b
				idx += 3
			}
		}
		return rgb
	}

	// Generic path for any image type
	for y := range h {
		for x := range w {
			r, g, b, _ := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			rgb[idx] = uint8(r >> 8)
			rgb[idx+1] = uint8(g >> 8)
			rgb[idx+2] = uint8(b >> 8)
			idx += 3
		}
	}
	return rgb
}

// ImageToYCbCr420 converts any image to YCbCr 4:2:0 for VP8 encoding.
// Only shortcuts if the input is already 4:2:0; otherwise does a full conversion.
func ImageToYCbCr420(img image.Image) *image.YCbCr {
	if ycbcr, ok := img.(*image.YCbCr); ok && ycbcr.SubsampleRatio == image.YCbCrSubsampleRatio420 {
		return ycbcr
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	out := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)

	for y := range h {
		for x := range w {
			r, g, b, _ := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			yy, cb, cr := color.RGBToYCbCr(uint8(r>>8), uint8(g>>8), uint8(b>>8))

			out.Y[y*out.YStride+x] = yy

			if x%2 == 0 && y%2 == 0 {
				ci := (y/2)*out.CStride + (x / 2)
				out.Cb[ci] = cb
				out.Cr[ci] = cr
			}
		}
	}
	return out
}
