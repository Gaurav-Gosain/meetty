package tui

// JoinResult carries the result of joining a room.
type JoinResult struct {
	PeerID       string // our own peer ID assigned by the server
	Participants []Participant
}

// HubProvider is the interface the TUI uses to interact with the server.
// Implemented by the LiveKit client.
type HubProvider interface {
	ListRooms() []RoomInfo
	CreateRoom(name, password string) error
	JoinRoom(sessionID, roomName, password string) (JoinResult, error)
	LeaveRoom(sessionID string)
	ToggleVideo(sessionID string)
	ToggleScreen(sessionID string)
	ToggleAudio(sessionID string)
	RequestDeviceList(sessionID, deviceType string)
	SelectDevice(sessionID, deviceType, deviceID string)
	SendChat(sessionID, message string)
}
