package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type lobbyState int

const (
	lobbyBrowsing lobbyState = iota
	lobbyCreating
	lobbyEnteringPassword
)

// LobbyModel is the room list / create room screen.
type LobbyModel struct {
	state    lobbyState
	rooms    []RoomInfo
	cursor   int
	input    string
	password string
	err      string
	width    int
	height   int
}

// NewLobbyModel creates a new lobby model.
func NewLobbyModel() LobbyModel {
	return LobbyModel{}
}

func (m LobbyModel) Init() tea.Cmd {
	return nil
}

func (m LobbyModel) update(msg tea.Msg, hub HubProvider, sessionID string) (LobbyModel, tea.Cmd) {
	switch msg := msg.(type) {
	case RoomListMsg:
		m.rooms = msg.Rooms
		if m.cursor >= len(m.rooms) {
			m.cursor = max(0, len(m.rooms)-1)
		}
		return m, nil

	case ErrorMsg:
		m.err = msg.Err.Error()
		return m, nil

	case tea.KeyPressMsg:
		m.err = ""

		switch m.state {
		case lobbyBrowsing:
			return m.updateBrowsing(msg, hub, sessionID)
		case lobbyCreating:
			return m.updateCreating(msg, hub)
		case lobbyEnteringPassword:
			return m.updatePassword(msg, hub, sessionID)
		}
	}

	return m, nil
}

func (m LobbyModel) updateBrowsing(msg tea.KeyPressMsg, hub HubProvider, sessionID string) (LobbyModel, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rooms)-1 {
			m.cursor++
		}
	case "n":
		m.state = lobbyCreating
		m.input = ""
	case "enter":
		if len(m.rooms) > 0 {
			room := m.rooms[m.cursor]
			if room.HasPassword {
				m.state = lobbyEnteringPassword
				m.password = ""
			} else {
				return m, joinRoom(hub, sessionID, room.Name, "")
			}
		}
	case "r":
		return m, func() tea.Msg {
			rooms := hub.ListRooms()
			return RoomListMsg{Rooms: rooms}
		}
	}
	return m, nil
}

func (m LobbyModel) updateCreating(msg tea.KeyPressMsg, hub HubProvider) (LobbyModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.state = lobbyBrowsing
		m.input = ""
	case "enter":
		if m.input != "" {
			name := m.input
			m.input = ""
			m.state = lobbyBrowsing
			return m, func() tea.Msg {
				if err := hub.CreateRoom(name, ""); err != nil {
					return ErrorMsg{Err: err}
				}
				return RoomListMsg{Rooms: hub.ListRooms()}
			}
		}
	case "backspace":
		if len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
	default:
		if len(msg.String()) == 1 {
			m.input += msg.String()
		}
	}
	return m, nil
}

func (m LobbyModel) updatePassword(msg tea.KeyPressMsg, hub HubProvider, sessionID string) (LobbyModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.state = lobbyBrowsing
		m.password = ""
	case "enter":
		if m.cursor >= len(m.rooms) {
			m.state = lobbyBrowsing
			m.password = ""
			return m, nil
		}
		room := m.rooms[m.cursor]
		pw := m.password
		m.password = ""
		m.state = lobbyBrowsing
		return m, joinRoom(hub, sessionID, room.Name, pw)
	case "backspace":
		if len(m.password) > 0 {
			m.password = m.password[:len(m.password)-1]
		}
	default:
		if len(msg.String()) == 1 {
			m.password += msg.String()
		}
	}
	return m, nil
}

func joinRoom(hub HubProvider, sessionID, roomName, password string) tea.Cmd {
	return func() tea.Msg {
		result, err := hub.JoinRoom(sessionID, roomName, password)
		if err != nil {
			return ErrorMsg{Err: err}
		}
		return JoinedRoomMsg{RoomName: roomName, PeerID: result.PeerID, Participants: result.Participants}
	}
}

// View renders the lobby screen.
func (m LobbyModel) View() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("  m e e t t y  [livekit]"))
	b.WriteString("\n")
	b.WriteString(subtitleStyle.Render("  video conferencing in your terminal"))
	b.WriteString("\n\n")

	switch m.state {
	case lobbyCreating:
		b.WriteString("  Room name: ")
		b.WriteString(inputStyle.Render(m.input + "_"))
		b.WriteString("\n\n")
		b.WriteString(helpStyle.Render("  enter to create | esc to cancel"))

	case lobbyEnteringPassword:
		b.WriteString("  Password: ")
		b.WriteString(inputStyle.Render(strings.Repeat("*", len(m.password)) + "_"))
		b.WriteString("\n\n")
		b.WriteString(helpStyle.Render("  enter to join | esc to cancel"))

	case lobbyBrowsing:
		if len(m.rooms) == 0 {
			b.WriteString(dimStyle.Render("  no rooms yet. press n to create one."))
			b.WriteString("\n")
		} else {
			b.WriteString(subtitleStyle.Render("  Available Rooms") + "\n\n")
			for i, room := range m.rooms {
				lock := " "
				if room.HasPassword {
					lock = "*"
				}

				label := fmt.Sprintf("%s %s (%d)", lock, room.Name, room.Count)
				if i == m.cursor {
					b.WriteString(selectedItemStyle.Render("▸ " + label))
				} else {
					b.WriteString(normalItemStyle.Render("  " + label))
				}
				b.WriteString("\n")
			}
		}

		b.WriteString("\n")
		if m.err != "" {
			b.WriteString(errorStyle.Render("  error: "+m.err) + "\n")
		}
		b.WriteString(helpStyle.Render("  n new | enter join | r refresh | q quit"))
	}

	return b.String()
}
