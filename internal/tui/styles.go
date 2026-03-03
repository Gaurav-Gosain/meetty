package tui

import "charm.land/lipgloss/v2"

// Color palette - inspired by tuios, organized by semantic purpose.
const (
	// Accents
	colorAccent    = "#4fc3f7" // bright cyan - primary accent
	colorAccent2   = "#66bb6a" // green - success/active
	colorHighlight = "#ffeb3b" // yellow - input cursor

	// Semantic
	colorError   = "#ef5350" // red
	colorSuccess = "#66bb6a" // green

	// Text
	colorFgSecondary = "#b0b0c0"
	colorFgMuted     = "#808090"
	colorFgDim       = "#505068"

	// Backgrounds
	colorBgBadge = "#1e3a5f" // dark blue for badges/selections

	// Borders
	colorBorder = "#3a3a5c" // subtle purple-gray
)

// Nerd font icons
const (
	iconMicOff = "󰍭" // nf-md-microphone_off
	iconScreen = "󰍹" // nf-md-monitor
	iconCamOff = "󰖠" // nf-md-video_off
	iconFlip   = "󰑐" // nf-md-flip_horizontal
	iconLeave  = "󰗼" // nf-md-exit_run
	iconCam    = "󰕧" // nf-md-video
	iconMic    = "󰍬" // nf-md-microphone
	iconUser   = ""  // nf-fa-user
)

// Lobby styles

var titleStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.Color(colorAccent)).
	MarginBottom(1)

var subtitleStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorFgSecondary))

var selectedItemStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#ffffff")).
	Background(lipgloss.Color(colorBgBadge)).
	Bold(true).
	Padding(0, 1)

var normalItemStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorFgSecondary)).
	Padding(0, 1)

var dimStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorFgMuted))

var errorStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorError)).
	Bold(true)

var inputStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorHighlight)).
	Bold(true)

var helpStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorFgMuted))

// Meeting styles

var roomTitleStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.Color(colorAccent))

var participantCountStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorFgMuted))

var nameStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorAccent)).
	Bold(true)

var nameSelfStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorAccent2)).
	Bold(true)

var badgeStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorAccent)).
	Background(lipgloss.Color(colorBgBadge)).
	Bold(true).
	Padding(0, 1)

var badgeMutedStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorError)).
	Background(lipgloss.Color(colorBgBadge)).
	Bold(true).
	Padding(0, 1)

var badgeActiveStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorSuccess)).
	Background(lipgloss.Color(colorBgBadge)).
	Bold(true).
	Padding(0, 1)

var placeholderStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorFgDim)).
	Bold(true)

var borderStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorBorder))

// Chat styles

var chatMsgStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorFgSecondary))

var chatSelfStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorAccent2))

var chatInputStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color(colorHighlight)).
	Bold(true)
