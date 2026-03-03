package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	livekit "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"github.com/Gaurav-Gosain/meetty/internal/tui"
)

// lkSampleWriter adapts *lksdk.LocalTrack to our SampleWriter interface.
type lkSampleWriter struct {
	track *lksdk.LocalTrack
}

func (w *lkSampleWriter) WriteSample(s media.Sample) error {
	return w.track.WriteSample(s, nil)
}

// LiveKitClient implements tui.HubProvider using the LiveKit Go SDK.
type LiveKitClient struct {
	lkURL    string // ws://localhost:7880
	apiURL   string // http://localhost:8080
	username string

	room       *lksdk.Room
	videoTrack *lksdk.LocalTrack
	audioTrack *lksdk.LocalTrack
	videoPub   *lksdk.LocalTrackPublication
	audioPub   *lksdk.LocalTrackPublication

	program interface{ Send(msg tea.Msg) }

	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.Mutex
	peerID        string
	roomName      string
	videoMuted    bool
	audioMuted    bool
	screenSharing bool

	captureMu    sync.Mutex
	cameraCancel context.CancelFunc
	cameraDone   chan struct{}
	screenCancel context.CancelFunc
	screenDone   chan struct{}
	audioCancel  context.CancelFunc
	cameraDevice string
	audioDevice  string
	videoEncoder *VP8Encoder
}

// NewLiveKitClient creates a client that connects to a LiveKit server.
func NewLiveKitClient(lkURL, apiURL, username string) *LiveKitClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &LiveKitClient{
		lkURL:    lkURL,
		apiURL:   apiURL,
		username: username,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// SetProgram sets the Bubble Tea program for sending messages to the TUI.
func (c *LiveKitClient) SetProgram(p interface{ Send(msg tea.Msg) }) {
	c.program = p
}

// Close shuts everything down.
func (c *LiveKitClient) Close() {
	c.stopAllCapture()
	c.cancel()
	if c.room != nil {
		c.room.Disconnect()
	}
}

// --- Token fetching ---

type tokenRequest struct {
	Room     string `json:"room"`
	Identity string `json:"identity"`
}

type tokenResponse struct {
	Token string `json:"token"`
}

func (c *LiveKitClient) fetchToken(room, identity string) (string, error) {
	body, _ := json.Marshal(tokenRequest{Room: room, Identity: identity})
	resp, err := http.Post(c.apiURL+"/token", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("fetch token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request failed: %s", resp.Status)
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	return tr.Token, nil
}

// --- HubProvider implementation ---

func (c *LiveKitClient) ListRooms() []tui.RoomInfo {
	resp, err := http.Get(c.apiURL + "/api/rooms")
	if err != nil {
		log.Printf("list rooms: %v", err)
		return nil
	}
	defer resp.Body.Close()

	var rooms []tui.RoomInfo
	if err := json.NewDecoder(resp.Body).Decode(&rooms); err != nil {
		log.Printf("decode rooms: %v", err)
		return nil
	}
	return rooms
}

func (c *LiveKitClient) CreateRoom(name, password string) error {
	body, _ := json.Marshal(map[string]string{"name": name, "password": password})
	resp, err := http.Post(c.apiURL+"/api/rooms", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create room: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("create room failed: %s", resp.Status)
	}
	return nil
}

func (c *LiveKitClient) JoinRoom(sessionID, roomName, password string) (tui.JoinResult, error) {
	token, err := c.fetchToken(roomName, c.username)
	if err != nil {
		return tui.JoinResult{}, err
	}

	room, err := lksdk.ConnectToRoomWithToken(c.lkURL, token, &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed:   c.onTrackSubscribed,
			OnTrackUnsubscribed: c.onTrackUnsubscribed,
			OnDataPacket:        c.onDataPacket,
		},
		OnParticipantConnected:    func(rp *lksdk.RemoteParticipant) { c.sendRoster() },
		OnParticipantDisconnected: func(rp *lksdk.RemoteParticipant) { c.sendRoster() },
		OnDisconnected: func() {
			if c.program != nil {
				c.program.Send(tui.ErrorMsg{Err: fmt.Errorf("disconnected from server")})
			}
		},
	})
	if err != nil {
		return tui.JoinResult{}, fmt.Errorf("connect to room: %w", err)
	}
	c.room = room

	c.mu.Lock()
	c.peerID = string(room.LocalParticipant.Identity())
	c.roomName = roomName
	c.mu.Unlock()

	// Create and publish video track
	videoTrack, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeVP8,
		ClockRate: 90000,
	})
	if err != nil {
		return tui.JoinResult{}, fmt.Errorf("create video track: %w", err)
	}
	c.videoTrack = videoTrack
	videoPub, err := room.LocalParticipant.PublishTrack(videoTrack, &lksdk.TrackPublicationOptions{
		Name:   "video",
		Source: livekit.TrackSource_CAMERA,
	})
	if err != nil {
		return tui.JoinResult{}, fmt.Errorf("publish video: %w", err)
	}
	c.videoPub = videoPub

	// Create and publish audio track
	// Opus in WebRTC/SDP is always negotiated with Channels=2 (stereo),
	// even when sending mono audio. Channels=1 fails codec matching.
	audioTrack, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	if err != nil {
		return tui.JoinResult{}, fmt.Errorf("create audio track: %w", err)
	}
	c.audioTrack = audioTrack
	audioPub, err := room.LocalParticipant.PublishTrack(audioTrack, &lksdk.TrackPublicationOptions{
		Name:   "audio",
		Source: livekit.TrackSource_MICROPHONE,
	})
	if err != nil {
		return tui.JoinResult{}, fmt.Errorf("publish audio: %w", err)
	}
	c.audioPub = audioPub

	// Start capture pipelines only after tracks are bound (WebRTC negotiation
	// complete). WriteSample silently drops data when packetizer is nil.
	videoTrack.OnBind(func() {
		log.Printf("video track bound, starting camera capture")
		c.startCamera()
	})
	audioTrack.OnBind(func() {
		log.Printf("audio track bound, starting audio capture")
		c.startAudio()
	})

	// Build initial roster
	participants := c.buildRoster()

	return tui.JoinResult{
		PeerID:       c.peerID,
		Participants: participants,
	}, nil
}

func (c *LiveKitClient) LeaveRoom(sessionID string) {
	c.stopAllCapture()
	if c.room != nil {
		c.room.Disconnect()
		c.room = nil
	}
	c.videoTrack = nil
	c.audioTrack = nil
	c.videoPub = nil
	c.audioPub = nil
	c.mu.Lock()
	c.roomName = ""
	c.videoMuted = false
	c.audioMuted = false
	c.screenSharing = false
	c.mu.Unlock()
}

func (c *LiveKitClient) ToggleVideo(sessionID string) {
	c.mu.Lock()
	c.videoMuted = !c.videoMuted
	muted := c.videoMuted
	c.mu.Unlock()

	if muted {
		c.stopCamera()
	} else {
		c.mu.Lock()
		sharing := c.screenSharing
		c.mu.Unlock()
		if !sharing {
			c.startCamera()
		}
	}

	// Mute/unmute the published track so LiveKit notifies other participants
	if c.videoPub != nil {
		c.videoPub.SetMuted(muted)
	}
	c.sendRoster()
}

func (c *LiveKitClient) ToggleScreen(sessionID string) {
	c.mu.Lock()
	c.screenSharing = !c.screenSharing
	sharing := c.screenSharing
	c.mu.Unlock()

	if sharing {
		c.stopCamera()
		c.startScreen()
	} else {
		c.stopScreen()
		c.mu.Lock()
		muted := c.videoMuted
		c.mu.Unlock()
		if !muted {
			c.startCamera()
		}
	}
	c.sendRoster()
}

func (c *LiveKitClient) ToggleAudio(sessionID string) {
	c.mu.Lock()
	c.audioMuted = !c.audioMuted
	muted := c.audioMuted
	c.mu.Unlock()

	if muted {
		c.stopAudio()
	} else {
		c.startAudio()
	}

	if c.audioPub != nil {
		c.audioPub.SetMuted(muted)
	}
	c.sendRoster()
}

func (c *LiveKitClient) RequestDeviceList(sessionID, deviceType string) {
	var devices []tui.DeviceItem
	if deviceType == "camera" {
		for _, cam := range ListCameras() {
			devices = append(devices, tui.DeviceItem{ID: cam.Path, Name: cam.Name})
		}
	} else if deviceType == "audio" {
		for _, dev := range ListAudioInputDevices() {
			devices = append(devices, tui.DeviceItem{ID: dev.Name, Name: dev.Name})
		}
	}
	if c.program != nil {
		c.program.Send(tui.DeviceListMsg{Type: deviceType, Devices: devices})
	}
}

func (c *LiveKitClient) SelectDevice(sessionID, deviceType, deviceID string) {
	switch deviceType {
	case "camera":
		c.captureMu.Lock()
		c.cameraDevice = deviceID
		c.captureMu.Unlock()

		c.mu.Lock()
		muted := c.videoMuted
		sharing := c.screenSharing
		c.mu.Unlock()

		if !muted && !sharing {
			c.stopCamera()
			c.startCamera()
		}
	case "audio":
		c.captureMu.Lock()
		c.audioDevice = deviceID
		c.captureMu.Unlock()

		c.mu.Lock()
		muted := c.audioMuted
		c.mu.Unlock()

		if !muted {
			c.stopAudio()
			c.startAudio()
		}
	}
	log.Printf("device selected: type=%s id=%s", deviceType, deviceID)
}

// --- Track subscription callbacks ---

func (c *LiveKitClient) onTrackSubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	senderID := string(rp.Identity())
	log.Printf("track subscribed: kind=%s sender=%s", track.Kind(), senderID)

	switch track.Kind() {
	case webrtc.RTPCodecTypeVideo:
		go c.handleRemoteVideoTrack(track, senderID)
	case webrtc.RTPCodecTypeAudio:
		go c.handleRemoteAudioTrack(track)
	}
}

func (c *LiveKitClient) onTrackUnsubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	log.Printf("track unsubscribed: kind=%s sender=%s", track.Kind(), string(rp.Identity()))
}

func (c *LiveKitClient) handleRemoteVideoTrack(track *webrtc.TrackRemote, senderID string) {
	decoder, err := NewVP8Decoder()
	if err != nil {
		log.Printf("VP8 decoder init failed: %v", err)
		return
	}
	defer decoder.Close()

	sb := samplebuilder.New(512, &codecs.VP8Packet{}, track.Codec().ClockRate)

	gotKeyframe := false
	decodeErrors := 0
	lastW, lastH := 0, 0
	minInterval := time.Second / time.Duration(tuiMaxFPS)
	var lastSend time.Time

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if c.ctx.Err() != nil {
				return // context cancelled, clean exit
			}
			log.Printf("video track read error: %v", err)
			return
		}

		sb.Push(pkt)

		for {
			sample := sb.Pop()
			if sample == nil {
				break
			}

			if len(sample.Data) < 3 {
				continue
			}

			isKeyframe := (sample.Data[0] & 0x01) == 0

			if !gotKeyframe && !isKeyframe {
				continue
			}

			if decodeErrors >= 5 {
				log.Printf("resetting VP8 decoder after %d errors", decodeErrors)
				decoder.Close()
				decoder, err = NewVP8Decoder()
				if err != nil {
					log.Printf("VP8 decoder re-init failed: %v", err)
					return
				}
				gotKeyframe = false
				decodeErrors = 0
				continue
			}

			// Throttle: if not enough time has passed, do a lightweight decode
			// (keeps decoder in sync) but skip the expensive YUV→RGB conversion.
			now := time.Now()
			if now.Sub(lastSend) < minInterval {
				w, h, syncErr := decoder.DecodeSync(sample.Data)
				if syncErr != nil {
					decodeErrors++
					gotKeyframe = false
					continue
				}
				if isKeyframe {
					gotKeyframe = true
				}
				if w > 0 && h > 0 {
					decodeErrors = 0
					if w != lastW || h != lastH {
						log.Printf("video resolution changed: %dx%d → %dx%d", lastW, lastH, w, h)
						lastW, lastH = w, h
					}
				}
				continue
			}

			rgb, width, height, err := decoder.Decode(sample.Data)
			if err != nil {
				decodeErrors++
				gotKeyframe = false
				log.Printf("VP8 decode error (%d): %v keyframe=%v", decodeErrors, err, isKeyframe)
				continue
			}
			if rgb == nil {
				continue
			}

			expected := width * height * 3
			if len(rgb) != expected {
				continue
			}

			if isKeyframe {
				gotKeyframe = true
			}
			decodeErrors = 0

			if width != lastW || height != lastH {
				log.Printf("video resolution changed: %dx%d → %dx%d", lastW, lastH, width, height)
				lastW, lastH = width, height
			}

			lastSend = now

			if c.program != nil {
				c.program.Send(tui.FrameMsg{
					SenderID: senderID,
					RGBData:  rgb,
					Width:    width,
					Height:   height,
				})
			}
		}
	}
}

func (c *LiveKitClient) handleRemoteAudioTrack(track *webrtc.TrackRemote) {
	player, err := NewOpusPlayer()
	if err != nil {
		log.Printf("audio player init failed: %v", err)
		return
	}
	defer player.Close()

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			log.Printf("audio track read error: %v", err)
			return
		}

		player.Play(pkt.Payload)
	}
}

// --- Capture lifecycle ---

// tuiMaxFPS is the max frame rate for sending frames to the TUI.
// Matches the TUI render cap; sending faster just wastes CPU and memory.
const tuiMaxFPS = 30

func (c *LiveKitClient) localFrameCallback() LocalFrameCallback {
	return func(rgb []byte, width, height int) {
		if c.program == nil {
			return
		}
		c.mu.Lock()
		pid := c.peerID
		c.mu.Unlock()
		if pid == "" {
			return
		}
		c.program.Send(tui.FrameMsg{
			SenderID: pid,
			RGBData:  rgb,
			Width:    width,
			Height:   height,
		})
	}
}

// localFrameThrottle returns a function that checks if a local frame should
// be sent to the TUI (returns true), or dropped (returns false).
// This allows skipping the expensive RawFrameToRGB conversion for dropped frames.
func (c *LiveKitClient) localFrameThrottle() func() bool {
	minInterval := time.Second / time.Duration(tuiMaxFPS)
	var lastSend time.Time
	return func() bool {
		now := time.Now()
		if now.Sub(lastSend) < minInterval {
			return false
		}
		lastSend = now
		return true
	}
}

func (c *LiveKitClient) startCamera() {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()

	if c.cameraCancel != nil {
		return
	}

	track := c.videoTrack
	if track == nil {
		return
	}

	ctx, cancel := context.WithCancel(c.ctx)
	c.cameraCancel = cancel
	done := make(chan struct{})
	c.cameraDone = done
	onFrame := c.localFrameCallback()
	onFrameThrottle := c.localFrameThrottle()
	onEncoder := func(enc *VP8Encoder) {
		c.captureMu.Lock()
		c.videoEncoder = enc
		c.captureMu.Unlock()
	}
	device := c.cameraDevice

	writer := &lkSampleWriter{track: track}
	go func() {
		defer close(done)
		// Try PipeWire raw path first
		if err := startCameraCapturePipelineRaw(ctx, CameraFPS, writer, onFrame, onFrameThrottle, onEncoder); err == nil || ctx.Err() != nil {
			return
		} else {
			log.Printf("PipeWire camera unavailable (%v), falling back to V4L2", err)
		}
		// Fall back to V4L2 JPEG pipeline
		if err := StartCapturePipeline(ctx, device, CameraFPS, writer, onFrame, onEncoder); err != nil {
			if ctx.Err() == nil {
				log.Printf("camera capture error: %v", err)
			}
		}
	}()
	log.Printf("camera capture started")
}

func (c *LiveKitClient) stopCamera() {
	c.captureMu.Lock()
	cancel := c.cameraCancel
	done := c.cameraDone
	c.cameraCancel = nil
	c.cameraDone = nil
	c.videoEncoder = nil
	c.captureMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (c *LiveKitClient) startScreen() {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()

	if c.screenCancel != nil {
		return
	}

	track := c.videoTrack
	if track == nil {
		return
	}

	ctx, cancel := context.WithCancel(c.ctx)
	c.screenCancel = cancel
	done := make(chan struct{})
	c.screenDone = done
	onFrame := c.localFrameCallback()
	onFrameThrottle := c.localFrameThrottle()
	onEncoder := func(enc *VP8Encoder) {
		c.captureMu.Lock()
		c.videoEncoder = enc
		c.captureMu.Unlock()
	}

	writer := &lkSampleWriter{track: track}
	go func() {
		defer close(done)
		if err := startScreenCapturePipeline(ctx, ScreenFPS, writer, onFrame, onFrameThrottle, onEncoder); err != nil {
			if ctx.Err() == nil {
				log.Printf("screen capture error: %v", err)
			}
		}
	}()
	log.Printf("screen capture started")
}

func (c *LiveKitClient) stopScreen() {
	c.captureMu.Lock()
	cancel := c.screenCancel
	done := c.screenDone
	c.screenCancel = nil
	c.screenDone = nil
	c.videoEncoder = nil
	c.captureMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (c *LiveKitClient) startAudio() {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()

	if c.audioCancel != nil {
		return
	}

	track := c.audioTrack
	if track == nil {
		return
	}

	ctx, cancel := context.WithCancel(c.ctx)
	c.audioCancel = cancel
	device := c.audioDevice

	writer := &lkSampleWriter{track: track}
	go func() {
		if err := StartAudioCapture(ctx, writer, device); err != nil {
			if ctx.Err() == nil {
				log.Printf("audio capture error: %v", err)
			}
		}
	}()
	log.Printf("audio capture started (device=%q)", device)
}

func (c *LiveKitClient) stopAudio() {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()

	if c.audioCancel != nil {
		c.audioCancel()
		c.audioCancel = nil
	}
}

func (c *LiveKitClient) stopAllCapture() {
	c.stopCamera()
	c.stopScreen()
	c.stopAudio()
}

// --- Roster management ---

func (c *LiveKitClient) buildRoster() []tui.Participant {
	if c.room == nil {
		return nil
	}

	var participants []tui.Participant

	// Add self
	c.mu.Lock()
	participants = append(participants, tui.Participant{
		ID:            c.peerID,
		Username:      c.username,
		VideoMuted:    c.videoMuted,
		ScreenSharing: c.screenSharing,
		AudioMuted:    c.audioMuted,
	})
	roomName := c.roomName
	c.mu.Unlock()

	// Add remote participants
	for _, rp := range c.room.GetRemoteParticipants() {
		p := tui.Participant{
			ID:       string(rp.Identity()),
			Username: string(rp.Identity()),
		}
		// Check track publication states
		for _, pub := range rp.TrackPublications() {
			if pub.Source() == livekit.TrackSource_CAMERA {
				p.VideoMuted = pub.IsMuted()
			}
			if pub.Source() == livekit.TrackSource_SCREEN_SHARE {
				p.ScreenSharing = !pub.IsMuted()
			}
			if pub.Source() == livekit.TrackSource_MICROPHONE {
				p.AudioMuted = pub.IsMuted()
			}
		}
		participants = append(participants, p)
	}

	_ = roomName
	return participants
}

func (c *LiveKitClient) sendRoster() {
	if c.program == nil {
		return
	}
	c.mu.Lock()
	roomName := c.roomName
	c.mu.Unlock()

	participants := c.buildRoster()
	c.program.Send(tui.RosterMsg{
		RoomName:     roomName,
		Participants: participants,
	})
}

// --- Chat ---

func (c *LiveKitClient) onDataPacket(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
	ud, ok := data.(*lksdk.UserDataPacket)
	if !ok || ud.Topic != "chat" {
		return
	}
	if c.program != nil {
		c.program.Send(tui.ChatRecvMsg{
			SenderID: params.SenderIdentity,
			Text:     string(ud.Payload),
		})
	}
}

func (c *LiveKitClient) SendChat(sessionID, message string) {
	if c.room == nil {
		return
	}
	err := c.room.LocalParticipant.PublishDataPacket(
		lksdk.UserData([]byte(message)),
		lksdk.WithDataPublishReliable(true),
		lksdk.WithDataPublishTopic("chat"),
	)
	if err != nil {
		log.Printf("send chat: %v", err)
	}
}

// --- Capture pipelines ---

// startCameraCapturePipelineRaw captures webcam via PipeWire raw frames and encodes to VP8.
// onFrameThrottle returns true when the TUI is ready for a new self-view frame;
// the expensive RawFrameToRGB conversion is skipped when it returns false.
func startCameraCapturePipelineRaw(ctx context.Context, fps int, writer SampleWriter, onLocalFrame LocalFrameCallback, onFrameThrottle func() bool, onEncoder EncoderCallback) error {
	rawCh := make(chan RawFrame, 2)

	pwErr := make(chan error, 1)
	go func() {
		pwErr <- CaptureWebcamPipeWireRaw(ctx, fps, rawCh)
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
		case err := <-pwErr:
			return err
		case frame := <-rawCh:
			ycbcr := RawFrameToYCbCr420(frame)
			if ycbcr == nil {
				continue
			}

			w := ycbcr.Rect.Dx()
			h := ycbcr.Rect.Dy()

			// Only do the expensive RGB conversion when the TUI will actually use it
			if onLocalFrame != nil && onFrameThrottle() {
				rgb, rgbW, rgbH := RawFrameToRGB(frame)
				if rgb != nil {
					onLocalFrame(rgb, rgbW, rgbH)
				}
			}

			var err error
			if encoder == nil || encoder.width != w || encoder.height != h {
				if encoder != nil {
					encoder.Close()
				}
				encoder, err = NewVP8Encoder(w, h, fps)
				if err != nil {
					log.Printf("VP8 encoder init failed for camera: %v", err)
					continue
				}
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
				log.Printf("write camera sample: %v", err)
			}
		}
	}
}

// startScreenCapturePipeline captures the screen, encodes to VP8, and writes samples.
func startScreenCapturePipeline(ctx context.Context, fps int, writer SampleWriter, onLocalFrame LocalFrameCallback, onFrameThrottle func() bool, onEncoder EncoderCallback) error {
	rawCh := make(chan RawFrame, 2)

	go func() {
		if err := CaptureScreenRaw(ctx, fps, rawCh); err != nil {
			if ctx.Err() == nil {
				log.Printf("screen capture error: %v", err)
			}
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
		case frame := <-rawCh:
			ycbcr := RawFrameToYCbCr420(frame)
			if ycbcr == nil {
				continue
			}

			w := ycbcr.Rect.Dx()
			h := ycbcr.Rect.Dy()

			if onLocalFrame != nil && onFrameThrottle() {
				rgb, rgbW, rgbH := RawFrameToRGB(frame)
				if rgb != nil {
					onLocalFrame(rgb, rgbW, rgbH)
				}
			}

			var err error
			if encoder == nil || encoder.width != w || encoder.height != h {
				if encoder != nil {
					encoder.Close()
				}
				encoder, err = NewVP8Encoder(w, h, fps)
				if err != nil {
					log.Printf("VP8 encoder init failed for screen: %v", err)
					continue
				}
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
				log.Printf("write screen sample: %v", err)
			}
		}
	}
}
