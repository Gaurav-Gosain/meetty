package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	lkconfig "github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	lkservice "github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/protocol/auth"
	livekit "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

const (
	defaultAPIKey    = "devkey"
	defaultAPISecret = "secret0123456789abcdef" // >=20 chars for HMAC
)

func main() {
	lkPort := flag.Int("lk-port", 7880, "LiveKit server port")
	apiPort := flag.Int("port", 8080, "API/web server port")
	apiKey := flag.String("api-key", defaultAPIKey, "LiveKit API key")
	apiSecret := flag.String("api-secret", defaultAPISecret, "LiveKit API secret")
	webDir := flag.String("web", "", "path to web directory (auto-detect if empty)")
	host := flag.String("host", "", "public IP or hostname (enables remote access; omit for localhost-only)")
	domain := flag.String("domain", "", "public domain name for browser WSS URLs (e.g. meetty.gaurav.zip)")
	flag.Parse()

	// --- Start embedded LiveKit server ---
	conf, err := lkconfig.NewConfig("", true, nil, nil)
	if err != nil {
		log.Fatalf("livekit config: %v", err)
	}
	conf.Port = uint32(*lkPort)
	conf.Development = true
	conf.Keys = map[string]string{*apiKey: *apiSecret}

	if *host != "" {
		conf.RTC.NodeIP = *host
		conf.RTC.EnableLoopbackCandidate = false
		conf.RTC.ICEPortRangeStart = 50000
		conf.RTC.ICEPortRangeEnd = 60000
	} else {
		conf.RTC.NodeIP = "127.0.0.1"
		conf.RTC.EnableLoopbackCandidate = true
	}

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		log.Fatalf("livekit node: %v", err)
	}

	if err := prometheus.Init(string(currentNode.NodeID()), currentNode.NodeType()); err != nil {
		log.Fatalf("prometheus init: %v", err)
	}

	lkServer, err := lkservice.InitializeServer(conf, currentNode)
	if err != nil {
		log.Fatalf("livekit init: %v", err)
	}

	go func() {
		if err := lkServer.Start(); err != nil {
			log.Fatalf("livekit server: %v", err)
		}
	}()

	// Wait for LiveKit to be ready
	for i := 0; i < 50; i++ {
		if lkServer.IsRunning() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !lkServer.IsRunning() {
		log.Fatal("livekit server failed to start")
	}

	lkURL := fmt.Sprintf("http://localhost:%d", *lkPort)
	lkHost := "localhost"
	if *host != "" {
		lkHost = *host
	}
	fmt.Printf("LiveKit server running on :%d\n", *lkPort)

	// --- Auto-detect web directory ---
	staticDir := *webDir
	if staticDir == "" {
		if info, err := os.Stat("web"); err == nil && info.IsDir() {
			staticDir = "web"
		} else if exe, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exe), "web")
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				staticDir = candidate
			}
		}
	}

	// --- HTTP API server ---
	roomClient := lksdk.NewRoomServiceClient(lkURL, *apiKey, *apiSecret)

	mux := http.NewServeMux()
	handler := corsMiddleware(mux)

	// Token endpoint
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Room     string `json:"room"`
			Identity string `json:"identity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.Room == "" || req.Identity == "" {
			http.Error(w, "room and identity required", http.StatusBadRequest)
			return
		}

		at := auth.NewAccessToken(*apiKey, *apiSecret)
		at.SetVideoGrant(&auth.VideoGrant{
			RoomJoin: true,
			Room:     req.Room,
		}).SetIdentity(req.Identity).
			SetValidFor(24 * time.Hour)

		token, err := at.ToJWT()
		if err != nil {
			http.Error(w, "token generation failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	})

	// List rooms
	mux.HandleFunc("GET /api/rooms", func(w http.ResponseWriter, r *http.Request) {
		res, err := roomClient.ListRooms(r.Context(), &livekit.ListRoomsRequest{})
		if err != nil {
			log.Printf("list rooms: %v", err)
			http.Error(w, "failed to list rooms", http.StatusInternalServerError)
			return
		}

		type roomInfo struct {
			Name        string `json:"Name"`
			HasPassword bool   `json:"HasPassword"`
			Count       int    `json:"Count"`
		}
		rooms := make([]roomInfo, 0, len(res.Rooms))
		for _, room := range res.Rooms {
			rooms = append(rooms, roomInfo{
				Name:  room.Name,
				Count: int(room.NumParticipants),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rooms)
	})

	// Create room
	mux.HandleFunc("POST /api/rooms", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name     string `json:"name"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}

		_, err := roomClient.CreateRoom(r.Context(), &livekit.CreateRoomRequest{
			Name:            req.Name,
			EmptyTimeout:    5 * 60,
			MaxParticipants: 50,
		})
		if err != nil {
			log.Printf("create room: %v", err)
			http.Error(w, "failed to create room", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
	})

	// LiveKit WebSocket URL for browser clients.
	// When --domain is set, Caddy proxies port 7443 (TLS) → 7880 with a real cert.
	// Falls back to --host IP with self-signed cert if no domain.
	wsURL := fmt.Sprintf("ws://%s:%d", lkHost, *lkPort)
	if *domain != "" {
		wsURL = fmt.Sprintf("wss://%s:7443", *domain)
	} else if *host != "" {
		wsURL = fmt.Sprintf("wss://%s:7443", lkHost)
	}
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"lkURL": wsURL,
		})
	})

	// Serve web frontend
	if staticDir != "" {
		mux.Handle("GET /", http.FileServer(http.Dir(staticDir)))
	}

	addr := fmt.Sprintf(":%d", *apiPort)
	fmt.Printf("API server listening on %s\n", addr)
	fmt.Printf("  Web UI: http://%s:%d\n", lkHost, *apiPort)
	fmt.Printf("  LiveKit WS: %s\n", wsURL)
	fmt.Printf("  Token:  POST http://%s:%d/token\n", lkHost, *apiPort)
	fmt.Printf("  Rooms:  GET  http://%s:%d/api/rooms\n", lkHost, *apiPort)

	httpServer := &http.Server{Addr: addr, Handler: handler}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() {
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	fmt.Println("\nshutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpServer.Shutdown(shutdownCtx)
	lkServer.Stop(true)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}
