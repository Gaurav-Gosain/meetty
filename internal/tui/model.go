package tui

import (
	"io"

	tea "charm.land/bubbletea/v2"
)

type screen int

const (
	screenLobby   screen = iota
	screenMeeting screen = iota
)

// Model is the root Bubble Tea model that routes between screens.
type Model struct {
	screen    screen
	lobby     LobbyModel
	meeting   MeetingModel
	hub       HubProvider
	sessionID string
	username  string
	width     int
	height    int
	output    io.Writer // direct reference to terminal output for kitty graphics
}

// NewModel creates a new root model.
func NewModel(hub HubProvider, sessionID, username string, output io.Writer) Model {
	return Model{
		screen:    screenLobby,
		hub:       hub,
		sessionID: sessionID,
		username:  username,
		output:    output,
		lobby:     NewLobbyModel(),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.lobby.Init(),
		m.refreshRooms(),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.lobby.width = msg.Width
		m.lobby.height = msg.Height
		if m.screen == screenMeeting {
			m.meeting.width = msg.Width
			m.meeting.height = msg.Height
			var cmd tea.Cmd
			m.meeting, cmd = m.meeting.Update(msg)
			return m, cmd
		}
		return m, nil

	case JoinedRoomMsg:
		m.screen = screenMeeting
		sid := m.sessionID
		if msg.PeerID != "" {
			sid = msg.PeerID
			m.sessionID = sid
		}
		m.meeting = NewMeetingModel(msg.RoomName, sid, m.username, m.hub, m.width, m.height, m.output)
		m.meeting.participants = msg.Participants
		return m, nil

	case LeftRoomMsg:
		m.screen = screenLobby
		return m, m.refreshRooms()

	case ErrorMsg:
		// Connection lost or server error while in meeting — return to lobby.
		if m.screen == screenMeeting {
			m.meeting.clearImagesSync()
			m.screen = screenLobby
			m.lobby.err = msg.Err.Error()
			hub := m.hub
			sid := m.sessionID
			return m, tea.Batch(
				func() tea.Msg {
					hub.LeaveRoom(sid)
					return nil
				},
				m.refreshRooms(),
			)
		}
		return m, nil

	case FrameMsg:
		if m.screen == screenMeeting {
			var cmd tea.Cmd
			m.meeting, cmd = m.meeting.Update(msg)
			return m, cmd
		}
		return m, nil

	case RosterMsg:
		if m.screen == screenMeeting {
			var cmd tea.Cmd
			m.meeting, cmd = m.meeting.Update(msg)
			return m, cmd
		}
		return m, nil

	case renderDoneMsg:
		if m.screen == screenMeeting {
			var cmd tea.Cmd
			m.meeting, cmd = m.meeting.Update(msg)
			return m, cmd
		}
		return m, nil

	case renderTickMsg:
		if m.screen == screenMeeting {
			var cmd tea.Cmd
			m.meeting, cmd = m.meeting.Update(msg)
			return m, cmd
		}
		return m, nil

	case DeviceListMsg:
		if m.screen == screenMeeting {
			var cmd tea.Cmd
			m.meeting, cmd = m.meeting.Update(msg)
			return m, cmd
		}
		return m, nil

	case ChatRecvMsg:
		if m.screen == screenMeeting {
			var cmd tea.Cmd
			m.meeting, cmd = m.meeting.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	switch m.screen {
	case screenLobby:
		var cmd tea.Cmd
		m.lobby, cmd = m.lobby.update(msg, m.hub, m.sessionID)
		return m, cmd

	case screenMeeting:
		var cmd tea.Cmd
		m.meeting, cmd = m.meeting.Update(msg)
		if m.meeting.leaving {
			// Clear images synchronously before screen transition to ensure
			// no kitty images persist when the lobby view renders.
			m.meeting.clearImagesSync()
			m.screen = screenLobby
			hub := m.hub
			sid := m.sessionID
			return m, tea.Batch(
				func() tea.Msg {
					hub.LeaveRoom(sid)
					return nil
				},
				m.refreshRooms(),
			)
		}
		return m, cmd
	}

	return m, nil
}

func (m Model) View() tea.View {
	var content string
	switch m.screen {
	case screenLobby:
		content = m.lobby.View()
	case screenMeeting:
		content = m.meeting.View()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	if m.screen == screenMeeting {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

func (m Model) refreshRooms() tea.Cmd {
	return func() tea.Msg {
		rooms := m.hub.ListRooms()
		tuiRooms := make([]RoomInfo, len(rooms))
		copy(tuiRooms, rooms)
		return RoomListMsg{Rooms: tuiRooms}
	}
}
