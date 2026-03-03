package tui

import (
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestGridSize(t *testing.T) {
	tests := []struct {
		n, termW, termH int
		wantCols        int
	}{
		{0, 80, 24, 1},
		{1, 80, 24, 1},
		{2, 160, 48, 2},
		{3, 160, 48, 3},
		{4, 160, 48, 2},
	}

	for _, tt := range tests {
		cols, rows := gridSize(tt.n, tt.termW, tt.termH)
		if cols != tt.wantCols {
			t.Errorf("gridSize(%d, %d, %d) cols = %d, want %d", tt.n, tt.termW, tt.termH, cols, tt.wantCols)
		}
		if rows <= 0 {
			t.Errorf("gridSize(%d, %d, %d) rows = %d, want > 0", tt.n, tt.termW, tt.termH, rows)
		}
		if cols*rows < tt.n && tt.n > 0 {
			t.Errorf("gridSize(%d, %d, %d) = %dx%d, can't fit %d participants", tt.n, tt.termW, tt.termH, cols, rows, tt.n)
		}
	}
}

func TestGridSize_ZeroTerminal(t *testing.T) {
	cols, rows := gridSize(2, 0, 0)
	if cols <= 0 || rows <= 0 {
		t.Errorf("gridSize with zero terminal should return defaults, got %dx%d", cols, rows)
	}
}

func TestCellDimensions(t *testing.T) {
	cellW, cellH, videoH := cellDimensions(160, 48, 2, 2)
	if cellW < 8 {
		t.Errorf("cellW = %d, want >= 8", cellW)
	}
	if cellH < 5 {
		t.Errorf("cellH = %d, want >= 5", cellH)
	}
	if videoH < 1 {
		t.Errorf("videoH = %d, want >= 1", videoH)
	}
	if videoH != cellH-3 {
		t.Errorf("videoH = %d, expected cellH-3 = %d", videoH, cellH-3)
	}

	cellW, cellH, videoH = cellDimensions(20, 10, 2, 2)
	if cellW < 8 || cellH < 5 || videoH < 1 {
		t.Errorf("small terminal: cellW=%d cellH=%d videoH=%d", cellW, cellH, videoH)
	}
}

func TestFitAspectRatio(t *testing.T) {
	tests := []struct {
		name                         string
		maxCols, maxRows, imgW, imgH int
	}{
		{"4:3 in wide cell", 40, 20, 320, 240},
		{"16:9 in wide cell", 40, 20, 1920, 1080},
		{"square in wide cell", 40, 20, 100, 100},
		{"tall image", 40, 20, 100, 400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cols, rows := fitAspectRatio(tt.maxCols, tt.maxRows, tt.imgW, tt.imgH)
			if cols <= 0 || rows <= 0 {
				t.Errorf("fitAspectRatio returned %dx%d, want positive", cols, rows)
			}
			if cols > tt.maxCols {
				t.Errorf("cols %d exceeds maxCols %d", cols, tt.maxCols)
			}
			if rows > tt.maxRows {
				t.Errorf("rows %d exceeds maxRows %d", rows, tt.maxRows)
			}
		})
	}
}

func TestFitAspectRatio_ScreenShareResolutions(t *testing.T) {
	// Screen share can produce very large images; ensure results are always
	// within bounds and positive.
	resolutions := [][2]int{
		{1920, 1080}, {2560, 1440}, {3840, 2160}, // common monitors
		{5120, 2880}, // 5K
		{1, 1080},    // degenerate narrow
		{1920, 1},    // degenerate short
	}
	cells := [][2]int{
		{78, 17}, {40, 10}, {200, 50}, {10, 3},
	}
	for _, res := range resolutions {
		for _, cell := range cells {
			cols, rows := fitAspectRatio(cell[0], cell[1], res[0], res[1])
			if cols <= 0 || rows <= 0 {
				t.Errorf("fitAspectRatio(%d,%d,%d,%d) = %dx%d, want positive",
					cell[0], cell[1], res[0], res[1], cols, rows)
			}
			if cols > cell[0] {
				t.Errorf("fitAspectRatio(%d,%d,%d,%d) cols=%d > maxCols=%d",
					cell[0], cell[1], res[0], res[1], cols, cell[0])
			}
			if rows > cell[1] {
				t.Errorf("fitAspectRatio(%d,%d,%d,%d) rows=%d > maxRows=%d",
					cell[0], cell[1], res[0], res[1], rows, cell[1])
			}
		}
	}
}

func TestFitAspectRatio_PreservesRatio(t *testing.T) {
	// For kitty graphics, fitAspectRatio should return cell dimensions
	// that match the image's aspect ratio (no 2x terminal cell factor).
	tests := []struct {
		name                         string
		maxCols, maxRows, imgW, imgH int
		wantCols, wantRows           int
	}{
		// 640x480 (4:3) in 80x20: rows=20, cols=20*2*640/480=53
		{"4:3 webcam", 80, 20, 640, 480, 53, 20},
		// 1920x1080 (16:9) in 80x20: rows=20, cols=20*2*1920/1080=71
		{"16:9 HD", 80, 20, 1920, 1080, 71, 20},
		// square in 80x20: rows=20, cols=20*2*1/1=40
		{"square", 80, 20, 100, 100, 40, 20},
		// very wide: rows=20, cols=20*2*800/100=320 > 40, so cols=40, rows=40*100/(2*800)=2
		{"wide image", 40, 20, 800, 100, 40, 2},
		// tall image in 80x20: rows=20, cols=20*2*100/400=10
		{"tall image", 80, 20, 100, 400, 10, 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cols, rows := fitAspectRatio(tt.maxCols, tt.maxRows, tt.imgW, tt.imgH)
			if cols != tt.wantCols || rows != tt.wantRows {
				t.Errorf("fitAspectRatio(%d, %d, %d, %d) = (%d, %d), want (%d, %d)",
					tt.maxCols, tt.maxRows, tt.imgW, tt.imgH,
					cols, rows, tt.wantCols, tt.wantRows)
			}
		})
	}
}

func TestFitAspectRatio_ZeroInputs(t *testing.T) {
	cols, rows := fitAspectRatio(0, 0, 320, 240)
	if cols <= 0 || rows <= 0 {
		t.Errorf("expected positive defaults, got %dx%d", cols, rows)
	}

	cols, rows = fitAspectRatio(40, 20, 0, 0)
	if cols <= 0 || rows <= 0 {
		t.Errorf("expected positive defaults, got %dx%d", cols, rows)
	}
}

func TestFlipRGBHorizontal(t *testing.T) {
	rgb := []byte{
		1, 2, 3,
		4, 5, 6,
		7, 8, 9,
	}
	flipRGBHorizontal(rgb, 3, 1)
	want := []byte{7, 8, 9, 4, 5, 6, 1, 2, 3}
	for i, v := range want {
		if rgb[i] != v {
			t.Errorf("rgb[%d] = %d, want %d", i, rgb[i], v)
		}
	}
}

func TestFlipRGBHorizontal_2x2(t *testing.T) {
	rgb := []byte{
		10, 20, 30, 40, 50, 60,
		70, 80, 90, 100, 110, 120,
	}
	flipRGBHorizontal(rgb, 2, 2)
	want := []byte{
		40, 50, 60, 10, 20, 30,
		100, 110, 120, 70, 80, 90,
	}
	for i, v := range want {
		if rgb[i] != v {
			t.Errorf("rgb[%d] = %d, want %d", i, rgb[i], v)
		}
	}
}

func TestFlipRGBHorizontal_DoubleFlip(t *testing.T) {
	original := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}
	rgb := make([]byte, len(original))
	copy(rgb, original)
	flipRGBHorizontal(rgb, 3, 1)
	flipRGBHorizontal(rgb, 3, 1)
	for i, v := range original {
		if rgb[i] != v {
			t.Errorf("double flip: rgb[%d] = %d, want %d", i, rgb[i], v)
		}
	}
}

func TestFlipRGBHorizontal_BoundsCheck(t *testing.T) {
	flipRGBHorizontal(nil, 0, 0)
	flipRGBHorizontal([]byte{1, 2, 3}, 1, 1)
	flipRGBHorizontal([]byte{1, 2}, 3, 1)
	flipRGBHorizontal([]byte{}, 0, 0)
	flipRGBHorizontal([]byte{1, 2, 3}, 0, 1)
}

func TestGridChanged(t *testing.T) {
	p1 := []Participant{{ID: "a"}}
	p2 := []Participant{{ID: "a"}, {ID: "b"}}

	if gridChanged(p1, p1) {
		t.Error("same participants should not report grid changed")
	}

	if !gridChanged(p1, p2) {
		t.Error("different participant count should report grid changed")
	}
}

func TestDecodeJPEGToRGB_InvalidData(t *testing.T) {
	rgb, w, h := decodeImageToRGB([]byte("not a jpeg"))
	if rgb != nil || w != 0 || h != 0 {
		t.Error("expected nil for invalid JPEG data")
	}
}

func TestDecodeJPEGToRGB_EmptyData(t *testing.T) {
	rgb, w, h := decodeImageToRGB(nil)
	if rgb != nil || w != 0 || h != 0 {
		t.Error("expected nil for nil data")
	}
}

func TestClampByte(t *testing.T) {
	tests := []struct {
		in   int32
		want byte
	}{
		{-100, 0},
		{0, 0},
		{128, 128},
		{255, 255},
		{300, 255},
	}
	for _, tt := range tests {
		got := clampByte(tt.in)
		if got != tt.want {
			t.Errorf("clampByte(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestPinnedLayout(t *testing.T) {
	mainW, mainH, sideW, sideH, sideCount := pinnedLayout(120, 40, 2)
	if mainW != 90 {
		t.Errorf("mainW = %d, want 90 (75%% of 120)", mainW)
	}
	if sideW != 30 {
		t.Errorf("sideW = %d, want 30 (25%% of 120)", sideW)
	}
	if mainH <= 0 || sideH <= 0 {
		t.Errorf("heights should be positive: mainH=%d sideH=%d", mainH, sideH)
	}
	if sideCount != 1 {
		t.Errorf("sideCount = %d, want 1", sideCount)
	}

	mainW, _, sideW, _, sideCount = pinnedLayout(120, 40, 1)
	if mainW != 120 {
		t.Errorf("solo pinned mainW = %d, want 120", mainW)
	}
	if sideW != 0 {
		t.Errorf("solo pinned sideW = %d, want 0", sideW)
	}
	if sideCount != 0 {
		t.Errorf("solo pinned sideCount = %d, want 0", sideCount)
	}
}

func TestHitTest_Grid(t *testing.T) {
	m := MeetingModel{
		width:  160,
		height: 48,
		participants: []Participant{
			{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
		},
		frames: make(map[string]*rgbFrame),
	}

	cols, _ := gridSize(4, 160, 48)
	cellW, cellH, _ := cellDimensions(160, 48, cols, (4+cols-1)/cols)

	got := m.hitTest(cellW/2, 1+cellH/2)
	if got != "a" {
		t.Errorf("hitTest center of cell 0 = %q, want 'a'", got)
	}

	got = m.hitTest(cellW+cellW/2, 1+cellH/2)
	if got != "b" {
		t.Errorf("hitTest center of cell 1 = %q, want 'b'", got)
	}

	got = m.hitTest(0, 1000)
	if got != "" {
		t.Errorf("hitTest outside grid = %q, want empty", got)
	}
}

func TestHitTest_Pinned(t *testing.T) {
	m := MeetingModel{
		width:    160,
		height:   48,
		pinnedID: "a",
		participants: []Participant{
			{ID: "a"}, {ID: "b"}, {ID: "c"},
		},
		frames: make(map[string]*rgbFrame),
	}

	got := m.hitTest(10, 10)
	if got != "a" {
		t.Errorf("hitTest pinned area = %q, want 'a'", got)
	}

	mainW, _, _, _, _ := pinnedLayout(160, 48, 3)
	got = m.hitTest(mainW+5, 2)
	if got != "b" && got != "c" {
		t.Errorf("hitTest sidebar = %q, want 'b' or 'c'", got)
	}
}

func TestHandleClick_PinUnpin(t *testing.T) {
	m := MeetingModel{
		width:  160,
		height: 48,
		participants: []Participant{
			{ID: "a"}, {ID: "b"},
		},
		frames: make(map[string]*rgbFrame),
	}

	cols, _ := gridSize(2, 160, 48)
	cellW, cellH, _ := cellDimensions(160, 48, cols, (2+cols-1)/cols)

	m.handleClick(cellW/2, 1+cellH/2)
	if m.pinnedID != "a" {
		t.Errorf("after first click pinnedID = %q, want 'a'", m.pinnedID)
	}

	m.handleClick(5, 5)
	if m.pinnedID != "" {
		t.Errorf("after second click pinnedID = %q, want empty", m.pinnedID)
	}
}

func TestPendingClear_ConsumedByRender(t *testing.T) {
	m := MeetingModel{
		width:        160,
		height:       48,
		pendingClear: true,
		pendingDels:  []uint32{1, 2, 3},
		participants: []Participant{{ID: "a"}},
		frames:       map[string]*rgbFrame{"a": {Data: []byte{1, 2, 3}, Width: 1, Height: 1}},
		imgBufs:      map[string]*imgBuf{"a": {ids: [2]uint32{10, 11}}},
		dirtyFrames:  make(map[string]bool),
		output:       &discardWriter{},
		outputMu:     &sync.Mutex{},
		rendering:    newRenderFlag(),
	}

	cmd := m.renderAllFrames()
	if m.pendingClear {
		t.Error("pendingClear should be consumed after renderAllFrames")
	}
	if m.pendingDels != nil {
		t.Errorf("pendingDels should be nil, got %v", m.pendingDels)
	}

	if cmd == nil {
		t.Error("renderAllFrames should return a non-nil command")
	}
}

func TestPickerOpensAndCloses(t *testing.T) {
	m := newTestMeeting()

	m.pickerType = "camera"
	m.pickerDevices = []DeviceItem{
		{ID: "0", Name: "USB Camera"},
		{ID: "1", Name: "Built-in"},
	}
	m.pickerCursor = 0

	m2, _ := m.updatePicker("down")
	if m2.pickerCursor != 1 {
		t.Errorf("pickerCursor = %d, want 1", m2.pickerCursor)
	}

	m2, _ = m2.updatePicker("up")
	if m2.pickerCursor != 0 {
		t.Errorf("pickerCursor = %d, want 0", m2.pickerCursor)
	}

	m2, _ = m2.updatePicker("esc")
	if m2.pickerType != "" {
		t.Errorf("pickerType = %q, want empty after esc", m2.pickerType)
	}
}

func TestPickerBoundsCheck(t *testing.T) {
	m := newTestMeeting()
	m.pickerType = "audio"
	m.pickerDevices = []DeviceItem{{ID: "0", Name: "Mic"}}
	m.pickerCursor = 0

	m2, _ := m.updatePicker("up")
	if m2.pickerCursor != 0 {
		t.Errorf("cursor went below 0: %d", m2.pickerCursor)
	}

	m2, _ = m2.updatePicker("down")
	if m2.pickerCursor != 0 {
		t.Errorf("cursor exceeded bounds: %d", m2.pickerCursor)
	}
}

func TestPickerSuppressesFrameRendering(t *testing.T) {
	m := newTestMeeting()
	m.pickerType = "camera"

	m2, cmd := m.Update(FrameMsg{SenderID: "a", RGBData: []byte{1, 2, 3}, Width: 1, Height: 1})
	if cmd != nil {
		t.Error("expected nil cmd when picker is open")
	}
	if _, ok := m2.frames["a"]; !ok {
		t.Error("frame data should be stored even with picker open")
	}
}

func TestForceRedraw(t *testing.T) {
	m := newTestMeeting()
	m.frames["a"] = &rgbFrame{Data: []byte{1, 2, 3}, Width: 1, Height: 1}
	m.imgBufs["a"] = &imgBuf{ids: [2]uint32{10, 11}}

	m.pendingClear = false
	m2, cmd := m.Update(keyMsg("r"))
	if !m2.pendingClear {
		if cmd == nil {
			t.Error("force redraw should produce a render command")
		}
	}
}

// mockHub implements HubProvider as no-ops for testing.
type mockHub struct{}

func (m *mockHub) ListRooms() []RoomInfo                               { return nil }
func (m *mockHub) CreateRoom(name, password string) error              { return nil }
func (m *mockHub) JoinRoom(sid, room, pw string) (JoinResult, error)   { return JoinResult{}, nil }
func (m *mockHub) LeaveRoom(sid string)                                {}
func (m *mockHub) ToggleVideo(sid string)                              {}
func (m *mockHub) ToggleScreen(sid string)                             {}
func (m *mockHub) ToggleAudio(sid string)                              {}
func (m *mockHub) RequestDeviceList(sid, deviceType string)            {}
func (m *mockHub) SelectDevice(sid, deviceType, deviceID string)       {}
func (m *mockHub) SendChat(sid, message string)                        {}

func newTestMeeting() MeetingModel {
	return MeetingModel{
		width:        160,
		height:       48,
		participants: []Participant{{ID: "a"}, {ID: "b"}},
		frames:       make(map[string]*rgbFrame),
		imgBufs:      make(map[string]*imgBuf),
		dirtyFrames:  make(map[string]bool),
		lastKeyTime:  make(map[string]time.Time),
		output:       &discardWriter{},
		outputMu:     &sync.Mutex{},
		rendering:    newRenderFlag(),
		hub:          &mockHub{},
	}
}

func keyMsg(key string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Text: key, Code: rune(key[0])}
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
