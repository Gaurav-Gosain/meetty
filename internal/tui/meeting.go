package tui

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/log/v2"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func newRenderFlag() *int32 {
	v := int32(0)
	return &v
}

// rgbPool reuses large RGB buffers to reduce GC pressure during image decode.
var rgbPool sync.Pool

// renderDoneMsg signals that a frame render completed.
type renderDoneMsg struct{}

// renderTickMsg signals that the render throttle interval has elapsed.
type renderTickMsg struct{}

// imgBuf holds a double-buffered pair of kitty image IDs for flicker-free rendering.
type imgBuf struct {
	ids    [2]uint32
	active int // 0 or 1: which ID is currently displayed
}

const keyCooldown = 200 * time.Millisecond
const maxParticipants = 50
const tuiRenderInterval = 33 * time.Millisecond // ~30 FPS cap for TUI rendering

// MeetingModel is the meeting room screen with video grid.
type MeetingModel struct {
	roomName     string
	sessionID    string
	username     string
	hub          HubProvider
	participants []Participant
	frames       map[string]*rgbFrame // latest decoded frame per sender ID
	imgBufs      map[string]*imgBuf   // sender ID -> double-buffered image IDs
	dirtyFrames  map[string]bool      // participants whose frame changed during render
	nextImgID    uint32               // next available kitty image ID
	width        int
	height       int
	leaving      bool
	output       io.Writer            // direct writer to terminal for kitty graphics
	outputMu     *sync.Mutex          // serializes all kitty writes
	rendering      *int32               // atomic flag: 1 = rendering in progress
	lastKeyTime    map[string]time.Time // rate limiting for toggle keys
	lastRenderTime time.Time            // when the last render was dispatched
	renderPending  bool                 // a render tick is already scheduled
	pendingClear bool                 // deferred: clear all images before next render
	pendingDels  []uint32             // deferred: delete these image IDs before next render
	pinnedID     string               // participant ID that is pinned (empty = no pin)
	// Device picker state
	pickerType    string       // "camera" or "audio" or "" (no picker)
	pickerDevices []DeviceItem // available devices
	pickerCursor  int          // selected device index
	// Chat state
	chatMessages []ChatMessage // recent chat messages (max 50)
	chatInput    string        // current chat input text
	chatMode     bool          // true when typing a chat message
}

// rgbFrame holds decoded RGB frame data.
type rgbFrame struct {
	Data   []byte
	Width  int
	Height int
}

// NewMeetingModel creates a new meeting model.
func NewMeetingModel(roomName, sessionID, username string, hub HubProvider, width, height int, output io.Writer) MeetingModel {
	return MeetingModel{
		roomName:    roomName,
		sessionID:   sessionID,
		username:    username,
		hub:         hub,
		frames:      make(map[string]*rgbFrame),
		imgBufs:     make(map[string]*imgBuf),
		dirtyFrames: make(map[string]bool),
		lastKeyTime: make(map[string]time.Time),
		nextImgID:   1,
		width:       width,
		height:      height,
		output:      output,
		outputMu:    &sync.Mutex{},
		rendering:   newRenderFlag(),
	}
}

func (m MeetingModel) Init() tea.Cmd {
	return nil
}

func (m MeetingModel) Update(msg tea.Msg) (MeetingModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		key := msg.String()

		// Chat mode key handling
		if m.chatMode {
			return m.updateChat(msg)
		}

		// Device picker key handling
		if m.pickerType != "" {
			return m.updatePicker(key)
		}

		switch key {
		case "q", "esc", "ctrl+c":
			m.leaving = true
			return m, nil
		case "c":
			m.chatMode = true
			m.chatInput = ""
			return m, m.clearImagesCmd()
		case "v", "s", "a":
			if last, ok := m.lastKeyTime[key]; ok && time.Since(last) < keyCooldown {
				return m, nil
			}
			m.lastKeyTime[key] = time.Now()

			hub := m.hub
			sid := m.sessionID
			switch key {
			case "v":
				return m, func() tea.Msg { hub.ToggleVideo(sid); return nil }
			case "s":
				return m, func() tea.Msg { hub.ToggleScreen(sid); return nil }
			case "a":
				return m, func() tea.Msg { hub.ToggleAudio(sid); return nil }
			}
		case "V":
			hub := m.hub
			sid := m.sessionID
			m.pickerType = "camera"
			m.pickerDevices = nil
			m.pickerCursor = 0
			return m, tea.Batch(
				func() tea.Msg { hub.RequestDeviceList(sid, "camera"); return nil },
				m.clearImagesCmd(),
			)
		case "A":
			hub := m.hub
			sid := m.sessionID
			m.pickerType = "audio"
			m.pickerDevices = nil
			m.pickerCursor = 0
			return m, tea.Batch(
				func() tea.Msg { hub.RequestDeviceList(sid, "audio"); return nil },
				m.clearImagesCmd(),
			)
		case "r":
			m.pendingClear = true
			return m, m.renderAllFrames()
		}

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			m.handleClick(msg.X, msg.Y)
			m.pendingClear = true
			return m, m.renderAllFrames()
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.pendingClear = true
		return m, m.renderAllFrames()

	case FrameMsg:
		m.frames[msg.SenderID] = &rgbFrame{
			Data:   msg.RGBData,
			Width:  msg.Width,
			Height: msg.Height,
		}
		if _, ok := m.imgBufs[msg.SenderID]; !ok {
			if m.nextImgID > 0xFFFFFFF0 {
				m.nextImgID = 1
			}
			m.imgBufs[msg.SenderID] = &imgBuf{
				ids: [2]uint32{m.nextImgID, m.nextImgID + 1},
			}
			m.nextImgID += 2
		}
		if m.pickerType != "" || m.chatMode {
			return m, nil
		}
		m.dirtyFrames[msg.SenderID] = true
		return m, m.scheduleRenderTick()

	case renderTickMsg:
		m.renderPending = false
		if len(m.dirtyFrames) == 0 || m.pickerType != "" || m.chatMode {
			return m, nil
		}
		if atomic.LoadInt32(m.rendering) != 0 {
			// Render in progress — reschedule
			return m, m.scheduleRenderTick()
		}
		m.lastRenderTime = time.Now()
		return m, m.renderDirtyFrames()

	case renderDoneMsg:
		if len(m.dirtyFrames) > 0 {
			return m, m.scheduleRenderTick()
		}
		return m, nil

	case DeviceListMsg:
		if m.pickerType == msg.Type {
			m.pickerDevices = msg.Devices
			m.pickerCursor = 0
		}
		return m, nil

	case ChatRecvMsg:
		m.chatMessages = append(m.chatMessages, ChatMessage{
			Sender: msg.SenderID,
			Text:   msg.Text,
		})
		if len(m.chatMessages) > 50 {
			m.chatMessages = m.chatMessages[len(m.chatMessages)-50:]
		}
		return m, nil

	case RosterMsg:
		oldParticipants := m.participants
		m.participants = msg.Participants
		activeIDs := make(map[string]bool, len(msg.Participants))
		for _, p := range m.participants {
			activeIDs[p.ID] = true
			if p.VideoMuted && !p.ScreenSharing {
				if buf, ok := m.imgBufs[p.ID]; ok {
					m.pendingDels = append(m.pendingDels, buf.ids[0], buf.ids[1])
					delete(m.imgBufs, p.ID)
					delete(m.frames, p.ID)
					delete(m.dirtyFrames, p.ID)
				}
			}
		}
		for id, buf := range m.imgBufs {
			if !activeIDs[id] {
				m.pendingDels = append(m.pendingDels, buf.ids[0], buf.ids[1])
				delete(m.imgBufs, id)
				delete(m.frames, id)
				delete(m.dirtyFrames, id)
			}
		}
		// Reset pinned participant if they left
		if m.pinnedID != "" && !activeIDs[m.pinnedID] {
			m.pinnedID = ""
		}
		if gridChanged(oldParticipants, m.participants) {
			m.pendingClear = true
			return m, m.renderAllFrames()
		}
		if len(m.pendingDels) > 0 {
			return m, m.flushPendingDels()
		}
		return m, nil
	}

	return m, nil
}

func gridChanged(old, new []Participant) bool {
	return len(old) != len(new)
}

// scheduleRenderTick returns a tea.Cmd that fires a renderTickMsg after the
// throttle interval, unless one is already pending.
func (m *MeetingModel) scheduleRenderTick() tea.Cmd {
	if m.renderPending {
		return nil
	}
	m.renderPending = true
	elapsed := time.Since(m.lastRenderTime)
	if elapsed >= tuiRenderInterval {
		// Enough time has passed — fire immediately
		return func() tea.Msg { return renderTickMsg{} }
	}
	// Wait for the remaining interval
	wait := tuiRenderInterval - elapsed
	return func() tea.Msg {
		time.Sleep(wait)
		return renderTickMsg{}
	}
}

// renderDirtyFrames renders all dirty frames in a single batch.
func (m *MeetingModel) renderDirtyFrames() tea.Cmd {
	if m.output == nil || len(m.participants) == 0 || len(m.dirtyFrames) == 0 {
		return nil
	}

	// Collect all dirty sender IDs and clear the map
	dirtyIDs := make([]string, 0, len(m.dirtyFrames))
	for id := range m.dirtyFrames {
		dirtyIDs = append(dirtyIDs, id)
		delete(m.dirtyFrames, id)
	}

	// Snapshot state for the render goroutine
	n := len(m.participants)
	participants := make([]Participant, n)
	copy(participants, m.participants)
	frames := make(map[string]*rgbFrame, len(dirtyIDs))
	renderIDs := make(map[string]renderInfo, len(dirtyIDs))

	for _, id := range dirtyIDs {
		frame := m.frames[id]
		if frame == nil || len(frame.Data) == 0 {
			continue
		}
		buf := m.imgBufs[id]
		if buf == nil {
			continue
		}
		frames[id] = frame
		back := 1 - buf.active
		renderIDs[id] = renderInfo{newID: buf.ids[back], oldID: buf.ids[buf.active]}
		buf.active = back
	}

	if len(frames) == 0 {
		return nil
	}

	doClear := m.pendingClear
	m.pendingClear = false
	pendingDels := m.pendingDels
	m.pendingDels = nil

	output := m.output
	outputMu := m.outputMu
	width := m.width
	height := m.height
	rendering := m.rendering
	pinnedID := m.pinnedID

	// If clearing, render ALL participants (not just dirty)
	if doClear {
		allFrames := make(map[string]*rgbFrame, len(m.frames))
		maps.Copy(allFrames, m.frames)
		allRenderIDs := make(map[string]renderInfo, len(m.imgBufs))
		for id, buf := range m.imgBufs {
			allRenderIDs[id] = renderInfo{newID: buf.ids[1-buf.active], oldID: buf.ids[buf.active]}
		}
		// Merge in our already-flipped dirty IDs
		for id, ri := range renderIDs {
			allRenderIDs[id] = ri
		}
		frames = allFrames
		renderIDs = allRenderIDs
	}

	return func() tea.Msg {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in renderDirtyFrames", "error", r)
			}
		}()
		atomic.StoreInt32(rendering, 1)
		defer atomic.StoreInt32(rendering, 0)

		var kb KittyBuilder
		kb.BeginSync()

		if doClear {
			kb.DeleteAll()
			if pinnedID != "" {
				renderPinnedLayout(&kb, participants, frames, renderIDs, pinnedID, width, height)
			} else {
				renderGridLayout(&kb, participants, frames, renderIDs, width, height)
			}
		} else {
			for _, id := range pendingDels {
				kb.DeleteByID(id)
			}

			// Render each dirty frame individually in its grid position
			for _, dirtyID := range dirtyIDs {
				frame := frames[dirtyID]
				ri, ok := renderIDs[dirtyID]
				if frame == nil || !ok {
					continue
				}

				idx := -1
				for i, p := range participants {
					if p.ID == dirtyID {
						idx = i
						break
					}
				}
				if idx == -1 {
					continue
				}

				if pinnedID != "" {
					mainW, mainH, sideW, _, sideCount := pinnedLayout(width, height, n)
					if dirtyID == pinnedID {
						innerW := max(1, mainW-2)
						videoH := max(1, mainH-3)
						renderFrame(&kb, ri.newID, frame, false, innerW, videoH, 1, 2)
					} else if sideCount > 0 && sideW >= 4 {
						usableH := max(1, height-4)
						sideH := max(3, usableH/sideCount)
						sideInnerW := max(1, sideW-2)
						si := 0
						for _, p := range participants {
							if p.ID == pinnedID {
								continue
							}
							if p.ID == dirtyID {
								videoH := max(1, sideH-3)
								x := mainW + 1
								y := 1 + si*sideH + 1
								renderFrame(&kb, ri.newID, frame, false, sideInnerW, videoH, x, y)
								break
							}
							si++
						}
					}
				} else {
					cols, _ := gridSize(n, width, height)
					cellW, cellH, videoH := cellDimensions(width, height, cols, (n+cols-1)/cols)
					innerW := max(1, cellW-2)
					col := idx % cols
					row := idx / cols
					x := col*cellW + 1
					y := 1 + row*cellH + 1
					renderFrame(&kb, ri.newID, frame, false, innerW, videoH, x, y)
				}
				kb.DeleteByID(ri.oldID)
			}
		}

		kb.EndSync()
		outputMu.Lock()
		_, err := output.Write(kb.Bytes())
		outputMu.Unlock()
		if err != nil {
			log.Error("kitty write failed", "error", err)
		}
		return renderDoneMsg{}
	}
}

func (m *MeetingModel) renderAllFrames() tea.Cmd {
	if m.output == nil || len(m.participants) == 0 {
		return nil
	}

	participants := make([]Participant, len(m.participants))
	copy(participants, m.participants)
	frames := make(map[string]*rgbFrame, len(m.frames))
	maps.Copy(frames, m.frames)
	renderIDs := make(map[string]renderInfo, len(m.imgBufs))
	for id, buf := range m.imgBufs {
		back := 1 - buf.active
		renderIDs[id] = renderInfo{newID: buf.ids[back], oldID: buf.ids[buf.active]}
		buf.active = back
	}

	doClear := m.pendingClear
	m.pendingClear = false
	pendingDels := m.pendingDels
	m.pendingDels = nil

	output := m.output
	outputMu := m.outputMu
	width := m.width
	height := m.height
	pinnedID := m.pinnedID

	return func() tea.Msg {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in renderAllFrames", "error", r)
			}
		}()

		var kb KittyBuilder
		kb.BeginSync()

		if doClear {
			kb.DeleteAll()
		} else {
			for _, id := range pendingDels {
				kb.DeleteByID(id)
			}
		}

		if pinnedID != "" {
			renderPinnedLayout(&kb, participants, frames, renderIDs, pinnedID, width, height)
		} else {
			renderGridLayout(&kb, participants, frames, renderIDs, width, height)
		}

		kb.EndSync()
		outputMu.Lock()
		_, err := output.Write(kb.Bytes())
		outputMu.Unlock()
		if err != nil {
			log.Error("kitty write failed", "error", err)
		}
		return renderDoneMsg{}
	}
}

type renderInfo struct {
	newID, oldID uint32
}

func renderGridLayout(kb *KittyBuilder, participants []Participant, frames map[string]*rgbFrame, renderIDs map[string]renderInfo, width, height int) {
	n := len(participants)
	cols, _ := gridSize(n, width, height)
	gridRows := (n + cols - 1) / cols
	cellW, cellH, videoH := cellDimensions(width, height, cols, gridRows)
	innerW := max(1, cellW-2)

	for i, p := range participants {
		frame := frames[p.ID]
		if frame == nil || len(frame.Data) == 0 {
			continue
		}
		ri, ok := renderIDs[p.ID]
		if !ok {
			continue
		}

		col := i % cols
		row := i / cols
		x := col*cellW + 1
		y := 1 + row*cellH + 1

		renderFrame(kb, ri.newID, frame, false, innerW, videoH, x, y)
	}
}

func renderPinnedLayout(kb *KittyBuilder, participants []Participant, frames map[string]*rgbFrame, renderIDs map[string]renderInfo, pinnedID string, width, height int) {
	n := len(participants)
	mainW, mainH, sideW, _, sideCount := pinnedLayout(width, height, n)

	for _, p := range participants {
		if p.ID != pinnedID {
			continue
		}
		frame := frames[p.ID]
		ri, ok := renderIDs[p.ID]
		if frame == nil || len(frame.Data) == 0 || !ok {
			break
		}
		innerW := max(1, mainW-2)
		videoH := max(1, mainH-3)
		renderFrame(kb, ri.newID, frame, false, innerW, videoH, 1, 2)
		break
	}

	if sideCount <= 0 || sideW < 4 {
		return
	}

	usableH := max(1, height-4)
	sideH := max(3, usableH/sideCount)
	sideInnerW := max(1, sideW-2)
	si := 0
	for _, p := range participants {
		if p.ID == pinnedID {
			continue
		}
		frame := frames[p.ID]
		ri, ok := renderIDs[p.ID]
		if frame == nil || len(frame.Data) == 0 || !ok {
			si++
			continue
		}
		videoH := max(1, sideH-3)
		x := mainW + 1
		y := 1 + si*sideH + 1
		renderFrame(kb, ri.newID, frame, false, sideInnerW, videoH, x, y)
		si++
	}
}

// renderFrame renders a single frame with aspect-ratio fitting and centering.
// In WebRTC mode, frames are always RGB data (decoded VP8).
func renderFrame(kb *KittyBuilder, imgID uint32, frame *rgbFrame, flipped bool, innerW, videoH, x, y int) {
	if frame == nil || len(frame.Data) == 0 {
		return
	}
	if innerW <= 0 || videoH <= 0 {
		return
	}

	rgbData := frame.Data
	imgW := frame.Width
	imgH := frame.Height

	// Validate RGB data matches declared dimensions to prevent kitty garbage.
	if imgW <= 0 || imgH <= 0 || len(rgbData) != imgW*imgH*3 {
		return
	}

	if flipped {
		// Make a copy before flipping to avoid mutating shared data
		flippedRGB := make([]byte, len(rgbData))
		copy(flippedRGB, rgbData)
		flipRGBHorizontal(flippedRGB, imgW, imgH)
		rgbData = flippedRGB
	}

	fitCols, fitRows := fitAspectRatio(innerW, videoH, imgW, imgH)
	if fitCols <= 0 || fitRows <= 0 {
		return
	}
	padX := (innerW - fitCols) / 2
	padY := (videoH - fitRows) / 2
	if padX < 0 {
		padX = 0
	}
	if padY < 0 {
		padY = 0
	}
	kb.MoveCursor(x+padX, y+padY)
	kb.TransmitRGB(imgID, rgbData, imgW, imgH, fitCols, fitRows)
}

func (m MeetingModel) clearImagesCmd() tea.Cmd {
	output := m.output
	outputMu := m.outputMu
	if output == nil {
		return nil
	}
	return func() tea.Msg {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in clearImagesCmd", "error", r)
			}
		}()
		var kb KittyBuilder
		kb.DeleteAll()
		outputMu.Lock()
		output.Write(kb.Bytes())
		outputMu.Unlock()
		return nil
	}
}

// clearImagesSync immediately clears all kitty images. Call this when leaving
// the meeting to ensure images are gone before the screen transition.
func (m *MeetingModel) clearImagesSync() {
	if m.output == nil {
		return
	}
	var kb KittyBuilder
	kb.DeleteAll()
	m.outputMu.Lock()
	m.output.Write(kb.Bytes())
	m.outputMu.Unlock()
}


func (m *MeetingModel) flushPendingDels() tea.Cmd {
	if m.output == nil || len(m.pendingDels) == 0 {
		return nil
	}
	dels := make([]uint32, len(m.pendingDels))
	copy(dels, m.pendingDels)
	m.pendingDels = m.pendingDels[:0]
	output := m.output
	outputMu := m.outputMu
	return func() tea.Msg {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in flushPendingDels", "error", r)
			}
		}()
		var kb KittyBuilder
		kb.BeginSync()
		for _, id := range dels {
			kb.DeleteByID(id)
		}
		kb.EndSync()
		outputMu.Lock()
		output.Write(kb.Bytes())
		outputMu.Unlock()
		return renderDoneMsg{}
	}
}

func (m *MeetingModel) handleClick(clickX, clickY int) {
	if len(m.participants) == 0 {
		return
	}
	id := m.hitTest(clickX, clickY)
	if id == "" {
		return
	}
	if m.pinnedID == id {
		m.pinnedID = ""
	} else {
		m.pinnedID = id
	}
}

func (m *MeetingModel) hitTest(clickX, clickY int) string {
	n := len(m.participants)
	if n == 0 {
		return ""
	}

	clickY -= 1

	if m.pinnedID != "" {
		return m.hitTestPinned(clickX, clickY, n)
	}

	cols, _ := gridSize(n, m.width, m.height)
	cellW, cellH, _ := cellDimensions(m.width, m.height, cols, (n+cols-1)/cols)

	col := clickX / cellW
	row := clickY / cellH
	if col < 0 || col >= cols {
		return ""
	}
	idx := row*cols + col
	if idx < 0 || idx >= n {
		return ""
	}
	return m.participants[idx].ID
}

func (m *MeetingModel) hitTestPinned(clickX, clickY, n int) string {
	mainW, _, sideW, _, _ := pinnedLayout(m.width, m.height, n)

	if clickX < mainW {
		return m.pinnedID
	}
	if clickX >= mainW && clickX < mainW+sideW {
		sideCount := n - 1
		if sideCount <= 0 {
			return ""
		}
		usableH := max(1, m.height-4)
		sideH := max(3, usableH/sideCount) // must match rendering sideH
		idx := clickY / sideH
		if idx >= sideCount {
			return ""
		}
		si := 0
		for _, p := range m.participants {
			if p.ID == m.pinnedID {
				continue
			}
			if si == idx {
				return p.ID
			}
			si++
		}
	}
	return ""
}

func pinnedLayout(termW, termH, n int) (mainW, mainH, sideW, sideH int, sideCount int) {
	sideCount = n - 1
	usableH := max(3, termH-4)

	if sideCount <= 0 {
		return termW, usableH, 0, 0, 0
	}

	mainW = termW * 3 / 4
	sideW = termW - mainW
	mainH = usableH
	sideH = usableH
	return
}

// isPNG checks if data starts with the PNG magic header.
func isPNG(data []byte) bool {
	return len(data) >= 4 && data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G'
}

// decodeImageToRGB decodes JPEG or PNG data to raw RGB pixels.
func decodeImageToRGB(data []byte) (rgb []byte, width, height int) {
	var img image.Image
	var err error
	if isPNG(data) {
		img, err = png.Decode(bytes.NewReader(data))
	} else {
		img, err = jpeg.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return nil, 0, 0
	}

	bounds := img.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()
	need := w * h * 3

	if pooled, ok := rgbPool.Get().(*[]byte); ok && len(*pooled) >= need {
		rgb = (*pooled)[:need]
	} else {
		rgb = make([]byte, need)
	}

	if ycbcr, ok := img.(*image.YCbCr); ok {
		i := 0
		for y := range h {
			for x := range w {
				yi := ycbcr.YOffset(x, y)
				ci := ycbcr.COffset(x, y)
				yy := int32(ycbcr.Y[yi])
				cb := int32(ycbcr.Cb[ci]) - 128
				cr := int32(ycbcr.Cr[ci]) - 128

				r := yy + (91881*cr)>>16
				g := yy - (22554*cb)>>16 - (46802*cr)>>16
				b := yy + (116130*cb)>>16

				rgb[i] = clampByte(r)
				rgb[i+1] = clampByte(g)
				rgb[i+2] = clampByte(b)
				i += 3
			}
		}
		return rgb, w, h
	}

	i := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			rgb[i] = byte(r >> 8)
			rgb[i+1] = byte(g >> 8)
			rgb[i+2] = byte(b >> 8)
			i += 3
		}
	}
	return rgb, w, h
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

func flipRGBHorizontal(rgb []byte, w, h int) {
	stride := w * 3
	if w <= 1 || h <= 0 || len(rgb) < stride*h {
		return
	}
	for y := range h {
		row := rgb[y*stride : (y+1)*stride]
		for l, r := 0, (w-1)*3; l < r; l, r = l+3, r-3 {
			row[l], row[r] = row[r], row[l]
			row[l+1], row[r+1] = row[r+1], row[l+1]
			row[l+2], row[r+2] = row[r+2], row[l+2]
		}
	}
}

func cellDimensions(termW, termH, gridCols, gridRows int) (cellW, cellH, videoH int) {
	usableH := max(3, termH-3)
	cellW = max(8, termW/gridCols)
	cellH = max(5, usableH/gridRows)
	videoH = max(1, cellH-3)
	return
}

func fitAspectRatio(maxCols, maxRows int, imageW, imageH int) (cols, rows int) {
	if maxCols <= 0 || maxRows <= 0 || imageW <= 0 || imageH <= 0 {
		return max(1, maxCols), max(1, maxRows)
	}

	// Terminal cells are ~2x taller than wide (typically 8px x 16px).
	// Multiply by 2 to compensate when converting pixel aspect ratio to cell counts.
	rows = maxRows
	cols = (rows * 2 * imageW) / imageH
	if cols <= maxCols {
		return max(1, cols), rows
	}

	cols = maxCols
	rows = (cols * imageH) / (2 * imageW)
	return cols, max(1, rows)
}

func (m MeetingModel) View() string {
	var b strings.Builder

	header := roomTitleStyle.Render(fmt.Sprintf(" %s  %s", iconUser, m.roomName))
	header += participantCountStyle.Render(fmt.Sprintf("  %d people", len(m.participants)))
	b.WriteString(header + "\n")

	lines := 1

	if m.pickerType != "" {
		b.WriteString("\n")
		m.viewPicker(&b)
	} else if m.chatMode {
		b.WriteString("\n")
		m.viewChat(&b)
	} else if len(m.participants) == 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  Waiting for participants..."))
		b.WriteString("\n")
	} else if m.pinnedID != "" {
		_ = m.viewPinned(&b, lines)
	} else {
		_ = m.viewGrid(&b, lines)
	}

	b.WriteString("\n")
	var statusBadges []string
	for _, p := range m.participants {
		if p.ID == m.sessionID {
			if p.VideoMuted {
				statusBadges = append(statusBadges, badgeMutedStyle.Render(iconCamOff+" CAM OFF"))
			}
			if p.ScreenSharing {
				statusBadges = append(statusBadges, badgeActiveStyle.Render(iconScreen+" SHARING"))
			}
			if p.AudioMuted {
				statusBadges = append(statusBadges, badgeMutedStyle.Render(iconMicOff+" MIC OFF"))
			}
			break
		}
	}
	footer := helpStyle.Render("  " + iconLeave + " q:leave  " + iconCam + " v/V:cam  " + iconScreen + " s:screen  " + iconMic + " a/A:mic  c:chat  r:redraw")
	if len(statusBadges) > 0 {
		footer += "  " + strings.Join(statusBadges, " ")
	}
	b.WriteString(footer)

	return b.String()
}

func (m MeetingModel) viewPicker(b *strings.Builder) {
	var title string
	if m.pickerType == "camera" {
		title = iconCam + "  Select Camera"
	} else {
		title = iconMic + "  Select Microphone"
	}
	b.WriteString(roomTitleStyle.Render(title) + "\n")

	if len(m.pickerDevices) == 0 {
		b.WriteString(dimStyle.Render("  Loading devices...") + "\n")
	} else {
		maxW := max(1, m.width-6)
		for i, dev := range m.pickerDevices {
			name := dev.Name
			if len(name) > maxW {
				name = name[:max(1, maxW-1)] + "~"
			}
			if i == m.pickerCursor {
				b.WriteString(selectedItemStyle.Render("▸ "+name) + "\n")
			} else {
				b.WriteString(normalItemStyle.Render("  "+name) + "\n")
			}
		}
	}

	b.WriteString(helpStyle.Render("  ↑↓:select  enter:confirm  esc:cancel"))
}

func (m MeetingModel) viewGrid(b *strings.Builder, lines int) int {
	n := len(m.participants)
	cols, rows := gridSize(n, m.width, m.height)
	cellW, _, videoH := cellDimensions(m.width, m.height, cols, rows)
	innerW := max(1, cellW-2)

	for row := range rows {
		if lines >= m.height-2 {
			break
		}
		lines = m.viewCellRow(b, lines, row, cols, n, cellW, innerW, videoH, m.participants)
	}
	return lines
}

func (m MeetingModel) viewPinned(b *strings.Builder, lines int) int {
	n := len(m.participants)
	mainW, mainH, sideW, _, sideCount := pinnedLayout(m.width, m.height, n)
	mainInnerW := max(1, mainW-2)
	mainVideoH := max(1, mainH-3)

	var pinned *Participant
	var sideParticipants []Participant
	for i := range m.participants {
		if m.participants[i].ID == m.pinnedID {
			pinned = &m.participants[i]
		} else {
			sideParticipants = append(sideParticipants, m.participants[i])
		}
	}
	if pinned == nil {
		return m.viewGrid(b, lines)
	}

	usableH := max(1, m.height-4)
	sideH := 0
	sideInnerW := 0
	sideVideoH := 0
	if sideCount > 0 && sideW >= 4 {
		sideH = max(3, usableH/sideCount)
		sideInnerW = max(1, sideW-2)
		sideVideoH = max(1, sideH-3)
	}

	totalRows := mainVideoH + 3
	for lineIdx := range totalRows {
		if lines >= m.height-2 {
			break
		}

		var left string
		switch {
		case lineIdx == 0:
			left = borderStyle.Render("╭" + strings.Repeat("─", mainInnerW) + "╮")
		case lineIdx == totalRows-1:
			left = borderStyle.Render("╰" + strings.Repeat("─", mainInnerW) + "╯")
		case lineIdx == totalRows-2:
			left = m.renderNameLabel(*pinned, mainInnerW)
		default:
			border := borderStyle.Render("│")
			_, hasFrame := m.frames[pinned.ID]
			if hasFrame && !pinned.VideoMuted {
				left = border + strings.Repeat(" ", mainInnerW) + border
			} else {
				vLine := lineIdx - 1
				if vLine == mainVideoH/2 {
					left = m.renderPlaceholder(*pinned, mainInnerW)
				} else {
					left = border + strings.Repeat(" ", mainInnerW) + border
				}
			}
		}

		var right string
		if sideCount > 0 && sideW >= 4 {
			sideLineOffset := lineIdx
			cellIdx := sideLineOffset / sideH
			cellLine := sideLineOffset % sideH

			if cellIdx < sideCount && cellIdx < len(sideParticipants) {
				sp := sideParticipants[cellIdx]
				switch {
				case cellLine == 0:
					right = borderStyle.Render("╭" + strings.Repeat("─", sideInnerW) + "╮")
				case cellLine == sideH-1:
					right = borderStyle.Render("╰" + strings.Repeat("─", sideInnerW) + "╯")
				case cellLine == sideH-2:
					right = m.renderNameLabel(sp, sideInnerW)
				default:
					border := borderStyle.Render("│")
					_, hasFrame := m.frames[sp.ID]
					if hasFrame && !sp.VideoMuted {
						right = border + strings.Repeat(" ", sideInnerW) + border
					} else {
						vLine := cellLine - 1
						if vLine == sideVideoH/2 {
							right = m.renderPlaceholder(sp, sideInnerW)
						} else {
							right = border + strings.Repeat(" ", sideInnerW) + border
						}
					}
				}
			} else {
				right = strings.Repeat(" ", sideW)
			}
		}

		b.WriteString(left + right + "\n")
		lines++
	}
	return lines
}

func (m MeetingModel) viewCellRow(b *strings.Builder, lines, row, cols, n, cellW, innerW, videoH int, participants []Participant) int {
	var topParts []string
	for col := range cols {
		idx := row*cols + col
		if idx >= n {
			topParts = append(topParts, strings.Repeat(" ", cellW))
		} else {
			topParts = append(topParts, borderStyle.Render("╭"+strings.Repeat("─", innerW)+"╮"))
		}
	}
	b.WriteString(strings.Join(topParts, "") + "\n")
	lines++

	for line := range videoH {
		if lines >= m.height-2 {
			break
		}
		var rowParts []string
		for col := range cols {
			idx := row*cols + col
			if idx >= n {
				rowParts = append(rowParts, strings.Repeat(" ", cellW))
				continue
			}

			participant := participants[idx]
			_, hasFrame := m.frames[participant.ID]
			border := borderStyle.Render("│")
			if hasFrame && !participant.VideoMuted {
				rowParts = append(rowParts, border+strings.Repeat(" ", innerW)+border)
			} else {
				if line == videoH/2 {
					rowParts = append(rowParts, m.renderPlaceholder(participant, innerW))
				} else {
					rowParts = append(rowParts, border+strings.Repeat(" ", innerW)+border)
				}
			}
		}
		b.WriteString(strings.Join(rowParts, "") + "\n")
		lines++
	}

	if lines < m.height-2 {
		var nameParts []string
		for col := range cols {
			idx := row*cols + col
			if idx >= n {
				nameParts = append(nameParts, strings.Repeat(" ", cellW))
				continue
			}
			nameParts = append(nameParts, m.renderNameLabel(participants[idx], innerW))
		}
		b.WriteString(strings.Join(nameParts, "") + "\n")
		lines++
	}

	if lines < m.height-2 {
		var botParts []string
		for col := range cols {
			idx := row*cols + col
			if idx >= n {
				botParts = append(botParts, strings.Repeat(" ", cellW))
			} else {
				botParts = append(botParts, borderStyle.Render("╰"+strings.Repeat("─", innerW)+"╯"))
			}
		}
		b.WriteString(strings.Join(botParts, "") + "\n")
		lines++
	}
	return lines
}

func (m MeetingModel) renderNameLabel(p Participant, innerW int) string {
	border := borderStyle.Render("│")
	label := p.Username
	isSelf := p.ID == m.sessionID
	if isSelf {
		label += " (you)"
	}

	var icons string
	if p.ScreenSharing {
		icons += " " + badgeActiveStyle.Render(iconScreen)
	}
	if p.AudioMuted {
		icons += " " + badgeMutedStyle.Render(iconMicOff)
	}
	if p.VideoMuted && !p.ScreenSharing {
		icons += " " + badgeMutedStyle.Render(iconCamOff)
	}
	if m.pinnedID == p.ID {
		icons += " " + badgeStyle.Render("PIN")
	}

	iconsW := lipgloss.Width(icons)
	maxLabel := max(0, innerW-iconsW-2)
	if len(label) > maxLabel && maxLabel > 0 {
		label = label[:max(0, maxLabel-1)] + "~"
	}

	var rendered string
	if isSelf {
		rendered = nameSelfStyle.Render(" "+label) + icons
	} else {
		rendered = nameStyle.Render(" "+label) + icons
	}
	visWidth := lipgloss.Width(rendered)
	pad := max(0, innerW-visWidth)
	return border + rendered + strings.Repeat(" ", pad) + border
}

func (m MeetingModel) updatePicker(key string) (MeetingModel, tea.Cmd) {
	switch key {
	case "esc", "q":
		m.pickerType = ""
		m.pickerDevices = nil
		m.pendingClear = true
		return m, m.renderAllFrames()
	case "up", "k":
		if m.pickerCursor > 0 {
			m.pickerCursor--
		}
		return m, nil
	case "down", "j":
		if m.pickerCursor < len(m.pickerDevices)-1 {
			m.pickerCursor++
		}
		return m, nil
	case "enter":
		if len(m.pickerDevices) > 0 && m.pickerCursor < len(m.pickerDevices) {
			selected := m.pickerDevices[m.pickerCursor]
			hub := m.hub
			sid := m.sessionID
			pickerType := m.pickerType
			m.pickerType = ""
			m.pickerDevices = nil
			m.pendingClear = true
			return m, tea.Batch(
				func() tea.Msg {
					hub.SelectDevice(sid, pickerType, selected.ID)
					return nil
				},
				m.renderAllFrames(),
			)
		}
		m.pickerType = ""
		m.pendingClear = true
		return m, m.renderAllFrames()
	}
	return m, nil
}

func (m MeetingModel) viewChat(b *strings.Builder) {
	b.WriteString(roomTitleStyle.Render("  Chat") + "\n")

	if len(m.chatMessages) == 0 {
		b.WriteString(dimStyle.Render("  No messages yet") + "\n")
	} else {
		// Show last N messages that fit (max 20)
		maxShow := 20
		start := len(m.chatMessages) - maxShow
		if start < 0 {
			start = 0
		}
		for _, msg := range m.chatMessages[start:] {
			if msg.Sender == m.username {
				b.WriteString(chatSelfStyle.Render("  "+msg.Sender+": "+msg.Text) + "\n")
			} else {
				b.WriteString(chatMsgStyle.Render("  "+msg.Sender+": "+msg.Text) + "\n")
			}
		}
	}

	b.WriteString("\n")
	b.WriteString(chatInputStyle.Render("  > "+m.chatInput+"_") + "\n")
	b.WriteString(helpStyle.Render("  enter:send  esc:close"))
}

func (m MeetingModel) updateChat(msg tea.KeyPressMsg) (MeetingModel, tea.Cmd) {
	key := msg.String()
	switch key {
	case "enter":
		if m.chatInput != "" {
			text := m.chatInput
			m.chatInput = ""
			hub := m.hub
			sid := m.sessionID
			m.chatMessages = append(m.chatMessages, ChatMessage{
				Sender: m.username,
				Text:   text,
			})
			if len(m.chatMessages) > 50 {
				m.chatMessages = m.chatMessages[len(m.chatMessages)-50:]
			}
			return m, func() tea.Msg { hub.SendChat(sid, text); return nil }
		}
		// Empty enter exits chat mode
		m.chatMode = false
		m.pendingClear = true
		return m, m.renderAllFrames()
	case "esc":
		m.chatMode = false
		m.chatInput = ""
		m.pendingClear = true
		return m, m.renderAllFrames()
	case "backspace":
		if len(m.chatInput) > 0 {
			runes := []rune(m.chatInput)
			m.chatInput = string(runes[:len(runes)-1])
		}
		return m, nil
	default:
		// Use Text field for printable characters (handles space, unicode, etc.)
		if msg.Text != "" {
			m.chatInput += msg.Text
		}
		return m, nil
	}
}

func (m MeetingModel) renderPlaceholder(p Participant, innerW int) string {
	border := borderStyle.Render("│")
	label := iconCamOff + "  no video"
	if p.VideoMuted {
		label = iconCamOff + "  cam off"
	}
	placeholder := placeholderStyle.Render(label)
	visW := lipgloss.Width(placeholder)
	pad := max(0, innerW-visW)
	return border + strings.Repeat(" ", pad/2) + placeholder + strings.Repeat(" ", pad-pad/2) + border
}

func gridSize(n int, termW, termH int) (cols, rows int) {
	if n <= 1 {
		return 1, 1
	}
	if n > maxParticipants {
		n = maxParticipants
	}
	if termW <= 0 {
		termW = 80
	}
	if termH <= 0 {
		termH = 24
	}

	bestCols := 1
	bestVideoArea := 0

	for c := 1; c <= n && c <= 8; c++ {
		r := (n + c - 1) / c
		cellW, _, videoH := cellDimensions(termW, termH, c, r)
		if cellW < 8 || videoH < 1 {
			continue
		}
		fitC, fitR := fitAspectRatio(cellW-2, videoH, 4, 3)
		area := fitC * fitR
		if area > bestVideoArea {
			bestVideoArea = area
			bestCols = c
		}
	}

	return bestCols, (n + bestCols - 1) / bestCols
}
