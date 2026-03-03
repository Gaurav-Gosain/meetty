package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/Gaurav-Gosain/meetty/internal/client"
	"github.com/Gaurav-Gosain/meetty/internal/tui"
)

func main() {
	server := flag.String("server", "localhost:8080", "meetty-server API address")
	lkAddr := flag.String("lk", "", "LiveKit WebSocket address (default: ws://server:7880)")
	username := flag.String("user", os.Getenv("USER"), "username")
	logFile := flag.String("log", "", "log file path (default: discard)")
	listDevices := flag.Bool("list-devices", false, "list available cameras and exit")
	flag.Parse()

	// Warm the audio device cache before any audio capture/playback starts.
	client.ListAudioInputDevices()

	if *listDevices {
		cameras := client.ListCameras()
		if len(cameras) == 0 {
			fmt.Println("No cameras found.")
		} else {
			for _, cam := range cameras {
				fmt.Printf("  %s  %s\n", cam.Path, cam.Name)
			}
		}
		return
	}

	// Redirect logs to file to prevent leaking into TUI
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open log file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		log.SetOutput(f)
	} else {
		log.SetOutput(io.Discard)
	}

	if *username == "" {
		*username = "anon"
	}

	// Derive LiveKit URL from server address if not specified.
	// The LiveKit SDK expects http:// and converts to ws:// internally.
	apiURL := "http://" + *server
	lkURL := *lkAddr
	if lkURL == "" {
		host := *server
		// Strip port from server address to replace with LiveKit port
		for i := len(host) - 1; i >= 0; i-- {
			if host[i] == ':' {
				host = host[:i]
				break
			}
		}
		lkURL = "http://" + host + ":7880"
	}

	lkClient := client.NewLiveKitClient(lkURL, apiURL, *username)
	defer lkClient.Close()

	p := tea.NewProgram(tui.NewModel(lkClient, *username, *username, os.Stdout))
	lkClient.SetProgram(p)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
