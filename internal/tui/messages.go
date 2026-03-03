package tui

// FrameMsg is sent when a new video frame arrives for a participant.
type FrameMsg struct {
	SenderID string
	Username string
	RGBData  []byte // decoded VP8 → raw RGB pixels
	Width    int
	Height   int
}

// RosterMsg is sent when the room participant list changes.
type RosterMsg struct {
	RoomName     string
	Participants []Participant
}

// RoomListMsg carries the list of available rooms.
type RoomListMsg struct {
	Rooms []RoomInfo
}

// RoomInfo is a summary of a room for display.
type RoomInfo struct {
	Name        string
	HasPassword bool
	Count       int
}

// JoinedRoomMsg signals that we successfully joined a room.
type JoinedRoomMsg struct {
	RoomName     string
	PeerID       string // our own peer ID assigned by the server
	Participants []Participant
}

// LeftRoomMsg signals that we left the room.
type LeftRoomMsg struct{}

// ErrorMsg carries an error message.
type ErrorMsg struct {
	Err error
}

// DeviceListMsg carries a list of available devices for the picker.
type DeviceListMsg struct {
	Type    string       // "camera" or "audio"
	Devices []DeviceItem // available devices
}

// DeviceItem is a single device in the picker.
type DeviceItem struct {
	ID   string
	Name string
}

// ChatRecvMsg is sent when a chat message is received from a participant.
type ChatRecvMsg struct {
	SenderID string
	Text     string
}

// ChatMessage is a stored chat message for display.
type ChatMessage struct {
	Sender string
	Text   string
}

