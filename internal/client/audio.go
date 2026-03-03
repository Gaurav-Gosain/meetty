package client

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/pion/webrtc/v4/pkg/media"
	"gopkg.in/hraban/opus.v2"
)

const (
	audioSampleRate = 48000
	audioChannels   = 1
	audioFrameSize  = 960 // 20ms at 48kHz
	opusBitrate     = 64000
)

// OpusPlayer decodes Opus and plays via malgo.
type OpusPlayer struct {
	decoder *opus.Decoder
	device  *malgo.Device
	ctx     *malgo.AllocatedContext
	pcmBuf  chan []byte
}

// NewOpusPlayer creates a new Opus decoder + audio output.
func NewOpusPlayer() (*OpusPlayer, error) {
	dec, err := opus.NewDecoder(audioSampleRate, audioChannels)
	if err != nil {
		return nil, fmt.Errorf("opus decoder: %w", err)
	}

	mctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("malgo init: %w", err)
	}

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Playback)
	deviceConfig.Playback.Format = malgo.FormatS16
	deviceConfig.Playback.Channels = audioChannels
	deviceConfig.SampleRate = audioSampleRate

	pcmBuf := make(chan []byte, 32)

	deviceConfig.Playback.ShareMode = malgo.Shared
	device, err := malgo.InitDevice(mctx.Context, deviceConfig, malgo.DeviceCallbacks{
		Data: func(outputSamples, inputSamples []byte, frameCount uint32) {
			select {
			case data := <-pcmBuf:
				n := copy(outputSamples, data)
				// Zero remaining samples if short
				for i := n; i < len(outputSamples); i++ {
					outputSamples[i] = 0
				}
			default:
				// Silence
				for i := range outputSamples {
					outputSamples[i] = 0
				}
			}
		},
	})
	if err != nil {
		mctx.Uninit()
		mctx.Free()
		return nil, fmt.Errorf("malgo device init: %w", err)
	}

	if err := device.Start(); err != nil {
		device.Uninit()
		mctx.Uninit()
		mctx.Free()
		return nil, fmt.Errorf("malgo device start: %w", err)
	}

	p := &OpusPlayer{
		decoder: dec,
		device:  device,
		ctx:     mctx,
	}

	p.pcmBuf = pcmBuf

	return p, nil
}

// Play decodes an Opus packet and queues it for playback.
func (p *OpusPlayer) Play(opusData []byte) {
	pcm := make([]int16, audioFrameSize*audioChannels)
	n, err := p.decoder.Decode(opusData, pcm)
	if err != nil {
		return
	}

	// Convert int16 to bytes (S16LE)
	buf := make([]byte, n*audioChannels*2)
	for i := 0; i < n*audioChannels; i++ {
		buf[i*2] = byte(pcm[i])
		buf[i*2+1] = byte(pcm[i] >> 8)
	}

	select {
	case p.pcmBuf <- buf:
	default:
	}
}

// Close stops playback and releases resources.
func (p *OpusPlayer) Close() {
	if p.device != nil {
		p.device.Uninit()
	}
	if p.ctx != nil {
		p.ctx.Uninit()
		p.ctx.Free()
	}
}

var _ = (*OpusPlayer)(nil)

// AudioDevice represents an available audio input device.
type AudioDevice struct {
	Name string
	ID   malgo.DeviceID
}

var (
	audioDeviceCache     []AudioDevice
	audioDeviceCacheOnce sync.Once
)

// ListAudioInputDevices returns available microphone/capture devices.
// The list is cached on first call to avoid creating concurrent malgo contexts
// which crash miniaudio.
func ListAudioInputDevices() []AudioDevice {
	audioDeviceCacheOnce.Do(func() {
		mctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
		if err != nil {
			return
		}
		infos, err := mctx.Devices(malgo.Capture)
		_ = mctx.Uninit()
		mctx.Free()
		if err != nil {
			return
		}
		for _, info := range infos {
			audioDeviceCache = append(audioDeviceCache, AudioDevice{
				Name: info.Name(),
				ID:   info.ID,
			})
		}
	})
	return audioDeviceCache
}

// findAudioDeviceID looks up a device ID by name from the cached device list.
func findAudioDeviceID(name string) *malgo.DeviceID {
	for _, dev := range ListAudioInputDevices() {
		if dev.Name == name {
			id := dev.ID
			return &id
		}
	}
	return nil
}

// StartAudioCapture captures microphone audio, encodes to Opus, and writes samples.
// If deviceName is non-empty, that specific device is used; otherwise the system default.
func StartAudioCapture(ctx context.Context, writer SampleWriter, deviceName string) error {
	enc, err := opus.NewEncoder(audioSampleRate, audioChannels, opus.AppVoIP)
	if err != nil {
		return fmt.Errorf("opus encoder: %w", err)
	}
	enc.SetBitrate(opusBitrate)

	mctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return fmt.Errorf("malgo init: %w", err)
	}
	defer func() {
		mctx.Uninit()
		mctx.Free()
	}()

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.Format = malgo.FormatS16
	deviceConfig.Capture.Channels = audioChannels
	deviceConfig.SampleRate = audioSampleRate

	// Use specific device if requested
	if deviceName != "" {
		if devID := findAudioDeviceID(deviceName); devID != nil {
			deviceConfig.Capture.DeviceID = devID.Pointer()
			log.Printf("audio capture using device: %s", deviceName)
		}
	}

	// Buffer incoming PCM bytes and emit exactly audioFrameSize-sample frames.
	// malgo callbacks deliver arbitrary chunk sizes, but Opus needs exact frame sizes.
	frameCh := make(chan []int16, 16)
	var bufMu sync.Mutex
	var pcmBuf []byte

	device, err := malgo.InitDevice(mctx.Context, deviceConfig, malgo.DeviceCallbacks{
		Data: func(outputSamples, inputSamples []byte, frameCount uint32) {
			if len(inputSamples) == 0 {
				return
			}
			bufMu.Lock()
			pcmBuf = append(pcmBuf, inputSamples...)
			// Each sample is 2 bytes (S16LE), need audioFrameSize samples per Opus frame
			frameBytes := audioFrameSize * audioChannels * 2
			for len(pcmBuf) >= frameBytes {
				chunk := make([]int16, audioFrameSize*audioChannels)
				for i := range chunk {
					chunk[i] = int16(pcmBuf[i*2]) | int16(pcmBuf[i*2+1])<<8
				}
				pcmBuf = pcmBuf[frameBytes:]
				bufMu.Unlock()
				select {
				case frameCh <- chunk:
				default:
				}
				bufMu.Lock()
			}
			bufMu.Unlock()
		},
	})
	if err != nil {
		return fmt.Errorf("malgo capture init: %w", err)
	}
	defer device.Uninit()

	if err := device.Start(); err != nil {
		return fmt.Errorf("malgo capture start: %w", err)
	}
	defer device.Stop()

	const frameDuration = 20 * time.Millisecond // 960 samples at 48kHz = 20ms

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pcm := <-frameCh:
			opusBuf := make([]byte, 1024)
			n, err := enc.Encode(pcm, opusBuf)
			if err != nil {
				log.Printf("opus encode error: %v", err)
				continue
			}

			if err := writer.WriteSample(media.Sample{
				Data:     opusBuf[:n],
				Duration: frameDuration,
			}); err != nil {
				log.Printf("write audio sample: %v", err)
			}
		}
	}
}
