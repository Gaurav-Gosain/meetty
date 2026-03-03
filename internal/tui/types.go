package tui

// Participant represents a user in a room.
type Participant struct {
	ID            string
	Username      string
	VideoMuted    bool
	ScreenSharing bool
	AudioMuted    bool
}
