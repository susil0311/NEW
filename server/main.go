package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
)

type Config struct {
	Port                string
	JWTSigningKey       string
	TokenIssuer         string
	TokenTTL            time.Duration
	CanvasUpstreamBase  string
	CanvasUpstreamToken string
	WSWriteTimeout      time.Duration
}

func loadConfig() Config {
	cfg := Config{
		Port:                envOrDefault("PORT", "8080"),
		JWTSigningKey:       envOrDefault("JWT_SIGNING_KEY", "dev-only-change-me"),
		TokenIssuer:         envOrDefault("TOKEN_ISSUER", "sonora-self-hosted"),
		TokenTTL:            parseMinutes(envOrDefault("TOKEN_TTL_MINUTES", "60")),
		CanvasUpstreamBase:  strings.TrimRight(envOrDefault("CANVAS_UPSTREAM_BASE_URL", "https://artwork-sonora.koiiverse.cloud"), "/"),
		CanvasUpstreamToken: strings.TrimSpace(os.Getenv("CANVAS_UPSTREAM_TOKEN")),
		WSWriteTimeout:      parseSeconds(envOrDefault("WS_WRITE_TIMEOUT_SECONDS", "3"), 3),
	}
	return cfg
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func parseMinutes(raw string) time.Duration {
	n, err := time.ParseDuration(raw + "m")
	if err != nil || n <= 0 {
		return 60 * time.Minute
	}
	return n
}

func parseSeconds(raw string, fallback int) time.Duration {
	n, err := time.ParseDuration(raw + "s")
	if err != nil || n <= 0 {
		return time.Duration(fallback) * time.Second
	}
	return n
}

type TokenResponse struct {
	Token     string `json:"token"`
	TokenType string `json:"tokenType"`
	ExpiresIn int64  `json:"expiresIn"`
	ExpiresAt int64  `json:"expiresAt"`
}

type AuthClaims struct {
	Scope string `json:"scope"`
	jwt.RegisteredClaims
}

func issueToken(cfg Config, scope string) (TokenResponse, error) {
	now := time.Now().UTC()
	expires := now.Add(cfg.TokenTTL)
	claims := AuthClaims{
		Scope: scope,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    cfg.TokenIssuer,
			Subject:   "sonora-client",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
			ID:        randomHex(16),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(cfg.JWTSigningKey))
	if err != nil {
		return TokenResponse{}, err
	}
	return TokenResponse{
		Token:     signed,
		TokenType: "Bearer",
		ExpiresIn: int64(math.Round(cfg.TokenTTL.Seconds())),
		ExpiresAt: expires.Unix(),
	}, nil
}

func validateToken(cfg Config, raw string, requiredScopes ...string) (*AuthClaims, error) {
	parsed, err := jwt.ParseWithClaims(raw, &AuthClaims{}, func(token *jwt.Token) (interface{}, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("unexpected jwt signing method")
		}
		return []byte(cfg.JWTSigningKey), nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := parsed.Claims.(*AuthClaims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid token")
	}
	if len(requiredScopes) == 0 {
		return claims, nil
	}
	scopeSet := map[string]bool{}
	for _, s := range strings.Fields(strings.ToLower(strings.TrimSpace(claims.Scope))) {
		scopeSet[s] = true
	}
	for _, required := range requiredScopes {
		if scopeSet[strings.ToLower(required)] {
			return claims, nil
		}
	}
	return nil, errors.New("insufficient scope")
}

func extractBearer(r *http.Request) (string, error) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return "", errors.New("missing authorization header")
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", errors.New("invalid authorization header")
	}
	return strings.TrimSpace(parts[1]), nil
}

type TogetherRoomSettings struct {
	AllowGuestsToAddTracks       bool `json:"allowGuestsToAddTracks"`
	AllowGuestsToControlPlayback bool `json:"allowGuestsToControlPlayback"`
	RequireHostApprovalToJoin    bool `json:"requireHostApprovalToJoin"`
}

type CreateSessionRequest struct {
	HostDisplayName string               `json:"hostDisplayName"`
	Settings        TogetherRoomSettings `json:"settings"`
}

type CreateSessionResponse struct {
	SessionID string               `json:"sessionId"`
	Code      string               `json:"code"`
	HostKey   string               `json:"hostKey"`
	GuestKey  string               `json:"guestKey"`
	WsURL     string               `json:"wsUrl"`
	Settings  TogetherRoomSettings `json:"settings"`
}

type ResolveSessionRequest struct {
	Code string `json:"code"`
}

type ResolveSessionResponse struct {
	SessionID string               `json:"sessionId"`
	GuestKey  string               `json:"guestKey"`
	WsURL     string               `json:"wsUrl"`
	Settings  TogetherRoomSettings `json:"settings"`
}

type TogetherParticipant struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	IsHost      bool   `json:"isHost"`
	IsPending   bool   `json:"isPending"`
	IsConnected bool   `json:"isConnected"`
}

type TogetherTrack struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Artists     []string `json:"artists"`
	DurationSec int      `json:"durationSec"`
	ThumbnailURL *string `json:"thumbnailUrl"`
}

type TogetherRoomState struct {
	SessionID              string               `json:"sessionId"`
	HostID                 string               `json:"hostId"`
	Participants           []TogetherParticipant `json:"participants"`
	Settings               TogetherRoomSettings `json:"settings"`
	Queue                  []TogetherTrack      `json:"queue"`
	QueueHash              string               `json:"queueHash"`
	CurrentIndex           int                  `json:"currentIndex"`
	IsPlaying              bool                 `json:"isPlaying"`
	PositionMs             int64                `json:"positionMs"`
	RepeatMode             int                  `json:"repeatMode"`
	ShuffleEnabled         bool                 `json:"shuffleEnabled"`
	SentAtElapsedRealtimeMs int64               `json:"sentAtElapsedRealtimeMs"`
}

type TogetherMessage struct {
	Type string `json:"type"`
	Raw  json.RawMessage
}

func (m *TogetherMessage) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	m.Type = envelope.Type
	m.Raw = data
	return nil
}

type ClientHello struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocolVersion"`
	SessionID       string `json:"sessionId"`
	SessionKey      string `json:"sessionKey"`
	ClientID        string `json:"clientId"`
	DisplayName     string `json:"displayName"`
}

type ServerWelcome struct {
	Type            string               `json:"type"`
	ProtocolVersion int                  `json:"protocolVersion"`
	SessionID       string               `json:"sessionId"`
	ParticipantID   string               `json:"participantId"`
	Role            string               `json:"role"`
	IsPending       bool                 `json:"isPending"`
	Settings        TogetherRoomSettings `json:"settings"`
}

type ServerError struct {
	Type      string  `json:"type"`
	SessionID *string `json:"sessionId,omitempty"`
	Message   string  `json:"message"`
	Code      *string `json:"code,omitempty"`
}

type RoomStateMessage struct {
	Type  string           `json:"type"`
	State TogetherRoomState `json:"state"`
}

type ControlRequest struct {
	Type          string          `json:"type"`
	SessionID     string          `json:"sessionId"`
	ParticipantID string          `json:"participantId"`
	Action        json.RawMessage `json:"action"`
}

type AddTrackRequest struct {
	Type          string       `json:"type"`
	SessionID     string       `json:"sessionId"`
	ParticipantID string       `json:"participantId"`
	Track         TogetherTrack `json:"track"`
	Mode          string       `json:"mode"`
}

type JoinRequestMessage struct {
	Type        string            `json:"type"`
	SessionID   string            `json:"sessionId"`
	Participant TogetherParticipant `json:"participant"`
}

type JoinDecisionMessage struct {
	Type          string `json:"type"`
	SessionID     string `json:"sessionId"`
	ParticipantID string `json:"participantId"`
	Approved      bool   `json:"approved"`
}

type ParticipantJoinedMessage struct {
	Type        string            `json:"type"`
	SessionID   string            `json:"sessionId"`
	Participant TogetherParticipant `json:"participant"`
}

type ParticipantLeftMessage struct {
	Type          string  `json:"type"`
	SessionID     string  `json:"sessionId"`
	ParticipantID string  `json:"participantId"`
	Reason        *string `json:"reason,omitempty"`
}

type HeartbeatPing struct {
	Type                  string `json:"type"`
	SessionID             string `json:"sessionId"`
	PingID                int64  `json:"pingId"`
	ClientElapsedRealtime int64  `json:"clientElapsedRealtimeMs"`
}

type HeartbeatPong struct {
	Type                  string `json:"type"`
	SessionID             string `json:"sessionId"`
	PingID                int64  `json:"pingId"`
	ClientElapsedRealtime int64  `json:"clientElapsedRealtimeMs"`
	ServerElapsedRealtime int64  `json:"serverElapsedRealtimeMs"`
}

type KickMessage struct {
	Type          string  `json:"type"`
	SessionID     string  `json:"sessionId"`
	ParticipantID string  `json:"participantId"`
	Reason        *string `json:"reason,omitempty"`
}

type BanMessage struct {
	Type          string  `json:"type"`
	SessionID     string  `json:"sessionId"`
	ParticipantID string  `json:"participantId"`
	Reason        *string `json:"reason,omitempty"`
}

type Room struct {
	SessionID string
	Code      string
	HostKey   string
	GuestKey  string
	Settings  TogetherRoomSettings
	HostID    string
	CreatedAt time.Time

	mu            sync.RWMutex
	participants  map[string]*Peer
	hostPeer      *Peer
	lastRoomState *TogetherRoomState
	banned        map[string]bool
}

type Peer struct {
	Participant TogetherParticipant
	Conn        *websocket.Conn
	IsHost      bool
	Approved    bool
	ClientID    string
	SessionKey  string
}

func (r *Room) snapshotParticipants() []TogetherParticipant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]TogetherParticipant, 0, len(r.participants))
	for _, p := range r.participants {
		participant := p.Participant
		participant.IsConnected = p.Conn != nil
		participant.IsPending = !p.IsHost && r.Settings.RequireHostApprovalToJoin && !p.Approved
		list = append(list, participant)
	}
	return list
}

func (r *Room) applyRoomStateFromHost(state TogetherRoomState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state.HostID = r.HostID
	state.Settings = r.Settings
	state.Participants = r.snapshotParticipantsUnlocked()
	r.lastRoomState = &state
}

func (r *Room) currentRoomState() *TogetherRoomState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lastRoomState != nil {
		copyState := *r.lastRoomState
		copyState.Participants = r.snapshotParticipantsUnlocked()
		copyState.Settings = r.Settings
		copyState.HostID = r.HostID
		return &copyState
	}
	return &TogetherRoomState{
		SessionID:               r.SessionID,
		HostID:                  r.HostID,
		Participants:            r.snapshotParticipantsUnlocked(),
		Settings:                r.Settings,
		Queue:                   []TogetherTrack{},
		QueueHash:               "",
		CurrentIndex:            0,
		IsPlaying:               false,
		PositionMs:              0,
		RepeatMode:              0,
		ShuffleEnabled:          false,
		SentAtElapsedRealtimeMs: elapsedRealtimeMs(),
	}
}

func (r *Room) snapshotParticipantsUnlocked() []TogetherParticipant {
	list := make([]TogetherParticipant, 0, len(r.participants))
	for _, p := range r.participants {
		participant := p.Participant
		participant.IsConnected = p.Conn != nil
		participant.IsPending = !p.IsHost && r.Settings.RequireHostApprovalToJoin && !p.Approved
		list = append(list, participant)
	}
	return list
}

type RoomStore struct {
	mu           sync.RWMutex
	bySessionID  map[string]*Room
	byCode       map[string]*Room
}

func newRoomStore() *RoomStore {
	return &RoomStore{
		bySessionID: map[string]*Room{},
		byCode:      map[string]*Room{},
	}
}

func (s *RoomStore) create(settings TogetherRoomSettings) *Room {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionID := randomHex(16)
	for s.bySessionID[sessionID] != nil {
		sessionID = randomHex(16)
	}
	code := randomCode(6)
	for s.byCode[strings.ToUpper(code)] != nil {
		code = randomCode(6)
	}
	hostKey := randomHex(20)
	guestKey := randomHex(20)
	hostID := randomHex(12)
	room := &Room{
		SessionID: sessionID,
		Code:      strings.ToUpper(code),
		HostKey:   hostKey,
		GuestKey:  guestKey,
		Settings:  settings,
		HostID:    hostID,
		CreatedAt: time.Now().UTC(),
		participants: map[string]*Peer{},
		banned: map[string]bool{},
	}
	s.bySessionID[room.SessionID] = room
	s.byCode[room.Code] = room
	return room
}

func (s *RoomStore) byCodeLookup(code string) *Room {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byCode[strings.ToUpper(strings.TrimSpace(code))]
}

func (s *RoomStore) bySessionLookup(id string) *Room {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bySessionID[strings.TrimSpace(id)]
}

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return time.Now().Format("150405.000000")
	}
	return hex.EncodeToString(buf)
}

func randomCode(size int) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, size)
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return strings.ToUpper(randomHex(size))[:size]
	}
	for i := 0; i < size; i++ {
		buf[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(buf)
}

func elapsedRealtimeMs() int64 {
	return time.Now().UnixMilli()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string, code ...string) {
	resp := map[string]any{
		"ok":    false,
		"error": message,
	}
	if len(code) > 0 && strings.TrimSpace(code[0]) != "" {
		resp["code"] = code[0]
	}
	writeJSON(w, status, resp)
}

func requireScopes(cfg Config, scopes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bearer, err := extractBearer(r)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "Unauthorized")
				return
			}
			claims, err := validateToken(cfg, bearer, scopes...)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "Unauthorized")
				return
			}
			ctx := context.WithValue(r.Context(), claimsContextKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type claimsContextKey struct{}

type Server struct {
	cfg      Config
	store    *RoomStore
	upgrader websocket.Upgrader
	httpClient *http.Client
}

func newServer(cfg Config) *Server {
	return &Server{
		cfg:   cfg,
		store: newRoomStore(),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.handleLandingPage)

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/v1/auth/token", s.handleIssueToken)

	protectedTogether := requireScopes(s.cfg, "together:rw")
	mux.Handle("/v1/together/sessions", protectedTogether(http.HandlerFunc(s.handleCreateSession)))
	mux.Handle("/v1/together/sessions/resolve", protectedTogether(http.HandlerFunc(s.handleResolveSession)))
	mux.Handle("/v1/together/ws", protectedTogether(http.HandlerFunc(s.handleTogetherWS)))

	protectedCanvas := requireScopes(s.cfg, "canvas:read")
	mux.Handle("/v1/canvas", protectedCanvas(http.HandlerFunc(s.handleCanvasProxy)))
	mux.Handle("/v1/canvas/health", protectedCanvas(http.HandlerFunc(s.handleCanvasHealth)))

	return cors(mux)
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLandingPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	html := `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width,initial-scale=1"/>
  <title>Sonora — Music, Reimagined</title>
  <link rel="preconnect" href="https://fonts.googleapis.com"/>
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin/>
  <link href="https://fonts.googleapis.com/css2?family=Syne:wght@400;500;600;700;800&family=DM+Sans:ital,opsz,wght@0,9..40,300;0,9..40,400;0,9..40,500;1,9..40,300&display=swap" rel="stylesheet"/>
  <style>
    :root {
      --c0: #06060a;
      --c1: #0b0b12;
      --c2: #111120;
      --c3: #1a1a2e;
      --text: #f0efff;
      --muted: #7a7a9d;
      --dim: #3a3a5c;
      --a1: #7c6fff;
      --a2: #c76fff;
      --a3: #6fcfff;
      --a4: #6fff9e;
      --r: 0.75rem;
      --rx: 1.5rem;
    }

    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
    html { scroll-behavior: smooth; }

    body {
      font-family: 'DM Sans', system-ui, sans-serif;
      background: var(--c0);
      color: var(--text);
      overflow-x: hidden;
      min-height: 100vh;
    }

    /* ─── NOISE GRAIN OVERLAY ─── */
    body::before {
      content: '';
      position: fixed; inset: 0; z-index: 1000; pointer-events: none;
      opacity: 0.032;
      background-image: url("data:image/svg+xml,%3Csvg viewBox='0 0 256 256' xmlns='http://www.w3.org/2000/svg'%3E%3Cfilter id='noise'%3E%3CfeTurbulence type='fractalNoise' baseFrequency='0.9' numOctaves='4' stitchTiles='stitch'/%3E%3C/filter%3E%3Crect width='100%25' height='100%25' filter='url(%23noise)'/%3E%3C/svg%3E");
      background-size: 200px 200px;
    }

    /* ─── ANIMATED BACKGROUND MESH ─── */
    .mesh {
      position: fixed; inset: 0; z-index: 0; pointer-events: none;
      overflow: hidden;
    }
    .mesh-orb {
      position: absolute;
      border-radius: 50%;
      filter: blur(120px);
      opacity: 0.18;
    }
    .mesh-orb:nth-child(1) {
      width: 800px; height: 800px;
      background: var(--a1);
      top: -300px; left: -200px;
      animation: orb1 20s ease-in-out infinite alternate;
    }
    .mesh-orb:nth-child(2) {
      width: 600px; height: 600px;
      background: var(--a2);
      top: 30%; right: -200px;
      animation: orb2 25s ease-in-out infinite alternate;
    }
    .mesh-orb:nth-child(3) {
      width: 500px; height: 500px;
      background: var(--a3);
      bottom: -100px; left: 30%;
      animation: orb3 18s ease-in-out infinite alternate;
    }
    @keyframes orb1 { to { transform: translate(120px, 80px) scale(1.2); } }
    @keyframes orb2 { to { transform: translate(-80px, 120px) scale(0.85); } }
    @keyframes orb3 { to { transform: translate(-60px, -80px) scale(1.15); } }

    /* ─── LAYOUT ─── */
    .wrap {
      position: relative; z-index: 1;
      max-width: 1100px; margin: 0 auto;
      padding: 0 28px;
    }

    /* ─── NAV ─── */
    nav {
      display: flex; align-items: center; justify-content: space-between;
      padding: 28px 0 0;
      animation: fadeD 0.6s ease both;
    }
    .nav-brand {
      display: flex; align-items: center; gap: 10px;
      font-family: 'Syne', sans-serif;
      font-weight: 800; font-size: 1.2rem;
      letter-spacing: -0.02em;
      text-decoration: none; color: var(--text);
    }
    .brand-icon {
      width: 36px; height: 36px; border-radius: 10px;
      background: linear-gradient(135deg, var(--a1), var(--a2));
      display: grid; place-items: center;
      box-shadow: 0 0 24px rgba(124,111,255,0.5);
      flex-shrink: 0;
    }
    .nav-badge {
      font-size: 0.72rem; font-weight: 500;
      letter-spacing: 0.04em;
      color: var(--muted);
      background: rgba(255,255,255,0.04);
      border: 1px solid rgba(255,255,255,0.08);
      padding: 5px 14px; border-radius: 99px;
    }

    /* ─── HERO ─── */
    .hero {
      padding: 100px 0 80px;
      text-align: center;
    }
    .hero-chip {
      display: inline-flex; align-items: center; gap: 8px;
      font-size: 0.78rem; font-weight: 500; letter-spacing: 0.08em;
      text-transform: uppercase;
      color: var(--a1);
      background: rgba(124,111,255,0.1);
      border: 1px solid rgba(124,111,255,0.25);
      padding: 6px 16px; border-radius: 99px;
      margin-bottom: 32px;
      animation: fadeU 0.7s 0.1s ease both;
    }
    .chip-dot {
      width: 6px; height: 6px; border-radius: 50%;
      background: var(--a1);
      box-shadow: 0 0 8px var(--a1);
      animation: pulse 2s ease-in-out infinite;
    }
    @keyframes pulse { 0%,100%{opacity:1;} 50%{opacity:0.3;} }

    .hero h1 {
      font-family: 'Syne', sans-serif;
      font-size: clamp(3rem, 7vw, 6rem);
      font-weight: 800;
      line-height: 1.0;
      letter-spacing: -0.04em;
      margin-bottom: 28px;
      animation: fadeU 0.7s 0.15s ease both;
    }
    .hero h1 .line2 {
      display: block;
      background: linear-gradient(100deg, var(--a1) 0%, var(--a2) 50%, var(--a3) 100%);
      -webkit-background-clip: text;
      -webkit-text-fill-color: transparent;
      background-clip: text;
    }
    .hero-sub {
      font-size: 1.15rem; font-weight: 300; line-height: 1.7;
      color: var(--muted);
      max-width: 560px; margin: 0 auto 44px;
      animation: fadeU 0.7s 0.2s ease both;
    }
    .hero-ctas {
      display: flex; justify-content: center; flex-wrap: wrap; gap: 14px;
      animation: fadeU 0.7s 0.25s ease both;
    }
    .btn-grad {
      display: inline-flex; align-items: center; gap: 9px;
      font-size: 0.9rem; font-weight: 600;
      padding: 14px 28px; border-radius: var(--r);
      background: linear-gradient(135deg, var(--a1), var(--a2));
      color: #fff; text-decoration: none;
      box-shadow: 0 6px 32px rgba(124,111,255,0.4);
      transition: transform 0.2s, box-shadow 0.2s;
    }
    .btn-grad:hover { transform: translateY(-3px); box-shadow: 0 12px 40px rgba(124,111,255,0.55); }
    .btn-outline {
      display: inline-flex; align-items: center; gap: 9px;
      font-size: 0.9rem; font-weight: 500;
      padding: 14px 28px; border-radius: var(--r);
      border: 1px solid rgba(255,255,255,0.12);
      color: var(--text); text-decoration: none;
      background: rgba(255,255,255,0.04);
      transition: border-color 0.2s, background 0.2s;
    }
    .btn-outline:hover { border-color: rgba(255,255,255,0.25); background: rgba(255,255,255,0.08); }

    /* ─── MOCKUP PLAYER ─── */
    .player-wrap {
      margin: 72px auto 0;
      max-width: 420px;
      animation: fadeU 0.8s 0.3s ease both;
    }
    .player {
      background: linear-gradient(160deg, rgba(26,26,46,0.95), rgba(11,11,18,0.98));
      border: 1px solid rgba(255,255,255,0.08);
      border-radius: 28px;
      padding: 28px;
      box-shadow: 0 40px 120px rgba(0,0,0,0.7), 0 0 0 1px rgba(255,255,255,0.04) inset;
      backdrop-filter: blur(20px);
      position: relative;
      overflow: hidden;
    }
    .player::before {
      content:'';
      position: absolute; inset: 0;
      background: radial-gradient(ellipse at 50% 0%, rgba(124,111,255,0.12) 0%, transparent 60%);
      pointer-events: none;
    }
    .player-art {
      width: 100%; aspect-ratio: 1;
      border-radius: 20px;
      background: linear-gradient(135deg, #1e1040, #2a0040, #001840, #001a30);
      margin-bottom: 20px;
      position: relative;
      overflow: hidden;
      box-shadow: 0 16px 48px rgba(0,0,0,0.5);
    }
    .player-art-inner {
      position: absolute; inset: 0;
      display: grid; place-items: center;
    }
    .vinyl {
      width: 160px; height: 160px; border-radius: 50%;
      background: conic-gradient(from 0deg, #1a1040, #0d0020, #001830, #0a0a1a, #1a1040);
      box-shadow: 0 0 60px rgba(124,111,255,0.3);
      animation: spin 12s linear infinite;
      position: relative;
    }
    .vinyl::after {
      content: '';
      position: absolute; inset: 35%;
      border-radius: 50%;
      background: radial-gradient(circle, #252535, #15151f);
      box-shadow: 0 0 0 6px rgba(124,111,255,0.2);
    }
    @keyframes spin { to { transform: rotate(360deg); } }
    .canvas-glow {
      position: absolute; inset: 0;
      background: radial-gradient(ellipse at 30% 70%, rgba(199,111,255,0.25), transparent 50%),
                  radial-gradient(ellipse at 80% 20%, rgba(111,207,255,0.2), transparent 50%);
    }
    .player-meta {
      margin-bottom: 20px;
    }
    .player-title {
      font-family: 'Syne', sans-serif;
      font-size: 1.1rem; font-weight: 700;
      margin-bottom: 4px;
    }
    .player-artist {
      font-size: 0.85rem; color: var(--muted);
    }
    .player-bar {
      height: 3px; border-radius: 99px;
      background: rgba(255,255,255,0.1);
      margin-bottom: 8px; overflow: hidden;
    }
    .player-progress {
      height: 100%; width: 62%;
      background: linear-gradient(90deg, var(--a1), var(--a2));
      border-radius: 99px;
    }
    .player-times {
      display: flex; justify-content: space-between;
      font-size: 0.72rem; color: var(--muted);
      margin-bottom: 24px;
    }
    .player-controls {
      display: flex; align-items: center; justify-content: center; gap: 28px;
    }
    .ctrl-btn {
      background: none; border: none; cursor: pointer;
      color: var(--muted); padding: 0;
      transition: color 0.2s, transform 0.15s;
    }
    .ctrl-btn:hover { color: var(--text); transform: scale(1.1); }
    .ctrl-play {
      width: 54px; height: 54px; border-radius: 50%;
      background: linear-gradient(135deg, var(--a1), var(--a2));
      border: none; cursor: pointer;
      display: grid; place-items: center;
      box-shadow: 0 6px 24px rgba(124,111,255,0.45);
      transition: transform 0.2s, box-shadow 0.2s;
      color: #fff;
    }
    .ctrl-play:hover { transform: scale(1.08); box-shadow: 0 10px 32px rgba(124,111,255,0.6); }

    /* Floating tags on player */
    .player-tag {
      position: absolute;
      font-size: 0.7rem; font-weight: 600;
      padding: 5px 11px; border-radius: 99px;
      backdrop-filter: blur(10px);
      border: 1px solid rgba(255,255,255,0.12);
      white-space: nowrap;
      animation: float 4s ease-in-out infinite;
    }
    .ptag-lyrics {
      top: 28px; right: -24px;
      background: rgba(124,111,255,0.2);
      color: var(--a1);
      animation-delay: 0s;
    }
    .ptag-canvas {
      bottom: 100px; right: -32px;
      background: rgba(199,111,255,0.2);
      color: var(--a2);
      animation-delay: 1.3s;
    }
    .ptag-together {
      bottom: 60px; left: -40px;
      background: rgba(111,255,158,0.15);
      color: var(--a4);
      animation-delay: 0.7s;
    }
    @keyframes float {
      0%,100% { transform: translateY(0px); }
      50% { transform: translateY(-8px); }
    }

    /* ─── SECTION SHARED ─── */
    section { padding: 96px 0; }
    .sec-eyebrow {
      display: inline-block;
      font-size: 0.72rem; font-weight: 700; letter-spacing: 0.12em;
      text-transform: uppercase; color: var(--a1);
      margin-bottom: 14px;
    }
    .sec-title {
      font-family: 'Syne', sans-serif;
      font-size: clamp(1.8rem, 3.5vw, 2.8rem);
      font-weight: 800; letter-spacing: -0.03em;
      line-height: 1.1; margin-bottom: 18px;
    }
    .sec-sub {
      font-size: 1rem; line-height: 1.7; color: var(--muted);
      max-width: 540px;
    }

    /* ─── FEATURES BENTO GRID ─── */
    .bento {
      display: grid;
      grid-template-columns: repeat(12, 1fr);
      grid-template-rows: auto;
      gap: 16px;
      margin-top: 56px;
    }
    .bcard {
      background: linear-gradient(160deg, rgba(17,17,32,0.9), rgba(11,11,18,0.95));
      border: 1px solid rgba(255,255,255,0.06);
      border-radius: var(--rx);
      padding: 28px;
      transition: border-color 0.3s, transform 0.3s, box-shadow 0.3s;
      overflow: hidden;
      position: relative;
    }
    .bcard:hover {
      border-color: rgba(124,111,255,0.25);
      transform: translateY(-4px);
      box-shadow: 0 20px 60px rgba(0,0,0,0.4);
    }
    .bcard::before {
      content: '';
      position: absolute; inset: 0; border-radius: var(--rx);
      background: radial-gradient(circle at var(--mx,50%) var(--my,50%), rgba(124,111,255,0.06) 0%, transparent 60%);
      opacity: 0; transition: opacity 0.3s;
      pointer-events: none;
    }
    .bcard:hover::before { opacity: 1; }

    .bc1 { grid-column: span 5; }
    .bc2 { grid-column: span 7; }
    .bc3 { grid-column: span 4; }
    .bc4 { grid-column: span 4; }
    .bc5 { grid-column: span 4; }
    .bc6 { grid-column: span 8; }
    .bc7 { grid-column: span 4; }

    @media (max-width: 900px) {
      .bc1,.bc2,.bc3,.bc4,.bc5,.bc6,.bc7 { grid-column: span 12; }
    }
    @media (min-width: 901px) and (max-width: 1080px) {
      .bc1,.bc2 { grid-column: span 6; }
      .bc3,.bc4,.bc5 { grid-column: span 4; }
    }

    .bcard-icon {
      width: 48px; height: 48px; border-radius: 14px;
      display: grid; place-items: center;
      font-size: 1.4rem;
      margin-bottom: 18px;
      background: rgba(255,255,255,0.05);
      border: 1px solid rgba(255,255,255,0.08);
    }
    .bcard h3 {
      font-family: 'Syne', sans-serif;
      font-size: 1.05rem; font-weight: 700;
      margin-bottom: 10px; letter-spacing: -0.01em;
    }
    .bcard p {
      font-size: 0.88rem; line-height: 1.65; color: var(--muted);
    }

    /* Waveform decoration in a bcard */
    .wave-vis {
      display: flex; align-items: flex-end; gap: 3px;
      height: 48px; margin-bottom: 18px;
    }
    .wv-bar {
      width: 5px; border-radius: 3px;
      background: linear-gradient(to top, var(--a1), var(--a2));
      animation: wvb var(--d,1s) ease-in-out infinite alternate;
      opacity: 0.8;
    }
    @keyframes wvb { from { height: 8px; } to { height: var(--h,30px); } }

    /* Lyrics preview */
    .lyric-lines { margin-bottom: 18px; }
    .lyric-line {
      font-size: 0.88rem; line-height: 2;
      color: var(--dim);
      transition: color 0.3s;
    }
    .lyric-line.active { color: var(--text); font-weight: 600; font-size: 0.95rem; }

    /* Together session */
    .together-users {
      display: flex; gap: -8px; margin-bottom: 18px;
      flex-wrap: wrap; gap: 6px;
    }
    .tu-avatar {
      width: 36px; height: 36px; border-radius: 50%;
      border: 2px solid var(--c1);
      display: grid; place-items: center;
      font-size: 0.8rem; font-weight: 700;
      background: linear-gradient(135deg, var(--a1), var(--a2));
      color: #fff;
    }
    .tu-avatar:nth-child(2) { background: linear-gradient(135deg,#ff6f9f,#ff9f6f); }
    .tu-avatar:nth-child(3) { background: linear-gradient(135deg,var(--a3),var(--a4)); }
    .tu-avatar.more {
      background: rgba(255,255,255,0.08);
      color: var(--muted);
      font-size: 0.72rem;
    }
    .session-code {
      display: inline-block;
      font-family: monospace; font-size: 1.4rem; font-weight: 700;
      letter-spacing: 0.25em;
      background: linear-gradient(90deg, var(--a1), var(--a2));
      -webkit-background-clip: text; background-clip: text;
      -webkit-text-fill-color: transparent;
    }

    /* Stats mini chart */
    .mini-chart {
      display: flex; align-items: flex-end; gap: 6px;
      height: 60px; margin-bottom: 18px;
    }
    .mc-bar {
      flex: 1; border-radius: 4px 4px 0 0;
      background: linear-gradient(to top, rgba(124,111,255,0.4), rgba(199,111,255,0.6));
      transition: height 0.6s ease;
    }

    /* ─── FEATURES LIST SECTION ─── */
    .feat-list {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
      gap: 12px;
      margin-top: 52px;
    }
    .feat-item {
      display: flex; align-items: flex-start; gap: 14px;
      padding: 18px 20px; border-radius: var(--r);
      background: rgba(255,255,255,0.03);
      border: 1px solid rgba(255,255,255,0.05);
      transition: background 0.2s, border-color 0.2s;
    }
    .feat-item:hover { background: rgba(255,255,255,0.06); border-color: rgba(124,111,255,0.2); }
    .feat-item-icon {
      font-size: 1.2rem; flex-shrink: 0; margin-top: 1px;
    }
    .feat-item-text h4 {
      font-size: 0.9rem; font-weight: 600; margin-bottom: 4px;
    }
    .feat-item-text p {
      font-size: 0.8rem; color: var(--muted); line-height: 1.55;
    }

    /* ─── CTA BANNER ─── */
    .cta-banner {
      margin: 32px 0 80px;
      padding: 64px 48px;
      border-radius: 28px;
      background: linear-gradient(135deg, rgba(124,111,255,0.15), rgba(199,111,255,0.1), rgba(111,207,255,0.08));
      border: 1px solid rgba(124,111,255,0.2);
      text-align: center;
      position: relative; overflow: hidden;
    }
    .cta-banner::before {
      content: '';
      position: absolute; inset: 0;
      background: radial-gradient(ellipse at 50% 0%, rgba(124,111,255,0.15), transparent 65%);
      pointer-events: none;
    }
    .cta-banner h2 {
      font-family: 'Syne', sans-serif;
      font-size: clamp(1.6rem, 3vw, 2.4rem);
      font-weight: 800; letter-spacing: -0.03em;
      margin-bottom: 14px;
    }
    .cta-banner p {
      color: var(--muted); font-size: 1rem; margin-bottom: 32px;
    }

    /* ─── SUPPORT SECTION ─── */
    .support-card {
      max-width: 520px; margin: 0 auto;
      background: linear-gradient(160deg, rgba(17,17,32,0.95), rgba(11,11,18,0.98));
      border: 1px solid rgba(111,255,158,0.15);
      border-radius: var(--rx);
      padding: 36px;
      text-align: center;
    }
    .support-emoji { font-size: 2.5rem; margin-bottom: 16px; }
    .support-card h3 {
      font-family: 'Syne', sans-serif;
      font-size: 1.4rem; font-weight: 800;
      margin-bottom: 12px; letter-spacing: -0.02em;
    }
    .support-card p { font-size: 0.9rem; color: var(--muted); margin-bottom: 28px; line-height: 1.7; }
    .upi-row {
      display: flex; align-items: center; gap: 12px;
      background: rgba(111,255,158,0.06);
      border: 1px solid rgba(111,255,158,0.2);
      border-radius: var(--r); padding: 14px 18px;
      margin-bottom: 16px; text-align: left;
    }
    .upi-val {
      font-family: monospace; font-size: 1.1rem; font-weight: 700;
      color: var(--a4); flex: 1; letter-spacing: 0.03em;
    }
    .upi-copy {
      font-size: 0.78rem; font-weight: 600;
      padding: 7px 16px; border-radius: 8px;
      border: 1px solid rgba(111,255,158,0.3);
      background: rgba(111,255,158,0.1);
      color: var(--a4); cursor: pointer;
      transition: background 0.2s;
      outline: none;
    }
    .upi-copy:hover { background: rgba(111,255,158,0.22); }
    .upi-hint { font-size: 0.78rem; color: var(--muted); }

    /* ─── FOOTER ─── */
    footer {
      border-top: 1px solid rgba(255,255,255,0.05);
      padding: 36px 0 48px;
      display: flex; flex-wrap: wrap;
      justify-content: space-between; align-items: center;
      gap: 16px; font-size: 0.82rem; color: var(--muted);
    }
    footer a { color: var(--muted); text-decoration: none; transition: color 0.2s; }
    footer a:hover { color: var(--text); }
    .footer-links { display: flex; gap: 24px; flex-wrap: wrap; }

    /* ─── ANIMATIONS ─── */
    @keyframes fadeU { from { opacity:0; transform:translateY(24px); } to { opacity:1; transform:none; } }
    @keyframes fadeD { from { opacity:0; transform:translateY(-14px); } to { opacity:1; transform:none; } }

    .reveal { opacity: 0; transform: translateY(28px); transition: opacity 0.6s ease, transform 0.6s ease; }
    .reveal.shown { opacity: 1; transform: none; }
  </style>
</head>
<body>

<div class="mesh">
  <div class="mesh-orb"></div>
  <div class="mesh-orb"></div>
  <div class="mesh-orb"></div>
</div>

<div class="wrap">

  <!-- NAV -->
  <nav>
    <a class="nav-brand" href="/">
      <div class="brand-icon">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none">
          <path d="M9 18V5l12-2v13" stroke="#fff" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/>
          <circle cx="6" cy="18" r="3" fill="#fff"/>
          <circle cx="18" cy="16" r="3" fill="#fff"/>
        </svg>
      </div>
      Sonora
    </a>
    <span class="nav-badge">Android App</span>
  </nav>

  <!-- HERO -->
  <div class="hero">
    <div class="hero-chip">
      <span class="chip-dot"></span>
      Free &amp; Open Source
    </div>
    <h1>
      Your Music.<br>
      <span class="line2">No Limits.</span>
    </h1>
    <p class="hero-sub">
      Sonora brings YouTube Music to life with an immersive, feature-packed Android experience.
      Lyrics, Canvas art, offline playback, Listen Together, and so much more.
    </p>
    <div class="hero-ctas">
      <a class="btn-grad" href="https://github.com/susil-kumar/Sonora" target="_blank" rel="noopener">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor"><path d="M12 2C6.477 2 2 6.484 2 12.017c0 4.425 2.865 8.18 6.839 9.504.5.092.682-.217.682-.483 0-.237-.008-.868-.013-1.703-2.782.605-3.369-1.343-3.369-1.343-.454-1.158-1.11-1.466-1.11-1.466-.908-.62.069-.608.069-.608 1.003.07 1.531 1.032 1.531 1.032.892 1.53 2.341 1.088 2.91.832.092-.647.35-1.088.636-1.338-2.22-.253-4.555-1.113-4.555-4.951 0-1.093.39-1.988 1.029-2.688-.103-.253-.446-1.272.098-2.65 0 0 .84-.27 2.75 1.026A9.564 9.564 0 0 1 12 6.844a9.59 9.59 0 0 1 2.504.337c1.909-1.296 2.747-1.027 2.747-1.027.546 1.379.202 2.398.1 2.651.64.7 1.028 1.595 1.028 2.688 0 3.848-2.339 4.695-4.566 4.943.359.309.678.92.678 1.855 0 1.338-.012 2.419-.012 2.747 0 .268.18.58.688.482A10.019 10.019 0 0 0 22 12.017C22 6.484 17.522 2 12 2z"/></svg>
        View on GitHub
      </a>
      <a class="btn-outline" href="https://github.com/susil-kumar/Sonora/releases" target="_blank" rel="noopener">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>
        Download APK
      </a>
    </div>

    <!-- PLAYER MOCKUP -->
    <div class="player-wrap" style="position:relative">
      <span class="player-tag ptag-lyrics">🎵 Synced Lyrics</span>
      <span class="player-tag ptag-canvas">🎨 Canvas Art</span>
      <span class="player-tag ptag-together">👥 Listen Together</span>
      <div class="player">
        <div class="player-art">
          <div class="canvas-glow"></div>
          <div class="player-art-inner">
            <div class="vinyl"></div>
          </div>
        </div>
        <div class="player-meta">
          <div class="player-title">Blinding Lights</div>
          <div class="player-artist">The Weeknd</div>
        </div>
        <div class="player-bar"><div class="player-progress"></div></div>
        <div class="player-times"><span>2:14</span><span>3:20</span></div>
        <div class="player-controls">
          <button class="ctrl-btn">
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polygon points="19 20 9 12 19 4 19 20"/><line x1="5" y1="19" x2="5" y2="5"/></svg>
          </button>
          <button class="ctrl-play">
            <svg width="22" height="22" viewBox="0 0 24 24" fill="currentColor"><rect x="6" y="4" width="4" height="16" rx="1"/><rect x="14" y="4" width="4" height="16" rx="1"/></svg>
          </button>
          <button class="ctrl-btn">
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><polygon points="5 4 15 12 5 20 5 4"/><line x1="19" y1="5" x2="19" y2="19"/></svg>
          </button>
        </div>
      </div>
    </div>
  </div>

  <!-- BENTO FEATURES -->
  <section class="reveal">
    <span class="sec-eyebrow">Features</span>
    <h2 class="sec-title">Everything music should be.</h2>
    <p class="sec-sub">Built with Kotlin and Jetpack Compose, Sonora is a complete rethink of how music apps should feel on Android.</p>

    <div class="bento">

      <!-- Lyrics card -->
      <div class="bcard bc1">
        <div class="lyric-lines">
          <div class="lyric-line">I've been tryna call</div>
          <div class="lyric-line active">I've been on my own for long enough</div>
          <div class="lyric-line">Maybe you can show me how to love</div>
          <div class="lyric-line">Maybe</div>
        </div>
        <div class="bcard-icon">🎵</div>
        <h3>Synced &amp; Translated Lyrics</h3>
        <p>Word-by-word karaoke sync via BetterLyrics and LrcLib, with auto-scroll, seek-on-tap, and offset controls.</p>
      </div>

      <!-- Canvas card -->
      <div class="bcard bc2">
        <div class="bcard-icon">🎨</div>
        <h3>Canvas &amp; Artwork</h3>
        <p>Animated Spotify-style Canvas videos play behind your music. Full-screen immersive mode transforms the now-playing screen into a visual experience.</p>
        <div style="margin-top:20px;height:80px;border-radius:14px;background:linear-gradient(135deg,rgba(124,111,255,0.3),rgba(199,111,255,0.3),rgba(111,207,255,0.25));position:relative;overflow:hidden;display:flex;align-items:center;justify-content:center;">
          <div style="position:absolute;inset:0;background:radial-gradient(ellipse at 40% 60%, rgba(199,111,255,0.4),transparent 55%),radial-gradient(ellipse at 80% 30%, rgba(111,207,255,0.35),transparent 50%);"></div>
          <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="rgba(255,255,255,0.8)" stroke-width="1.5" stroke-linecap="round"><polygon points="5 3 19 12 5 21 5 3"/></svg>
        </div>
      </div>

      <!-- Audio quality -->
      <div class="bcard bc3">
        <div class="wave-vis">
          <div class="wv-bar" style="--h:22px;--d:0.9s"></div>
          <div class="wv-bar" style="--h:38px;--d:1.1s"></div>
          <div class="wv-bar" style="--h:30px;--d:0.8s"></div>
          <div class="wv-bar" style="--h:48px;--d:1.3s"></div>
          <div class="wv-bar" style="--h:28px;--d:1.0s"></div>
          <div class="wv-bar" style="--h:44px;--d:0.7s"></div>
          <div class="wv-bar" style="--h:34px;--d:1.4s"></div>
          <div class="wv-bar" style="--h:18px;--d:0.95s"></div>
        </div>
        <div class="bcard-icon">🎛️</div>
        <h3>Audio Engine</h3>
        <p>Equalizer, bass boost, 5-band EQ, and high-bitrate streaming up to 256kbps.</p>
      </div>

      <!-- Listen Together -->
      <div class="bcard bc4">
        <div class="together-users">
          <div class="tu-avatar">S</div>
          <div class="tu-avatar">A</div>
          <div class="tu-avatar">R</div>
          <div class="tu-avatar more">+4</div>
        </div>
        <div class="session-code">K7MN2P</div>
        <div style="margin:12px 0 18px;font-size:0.78rem;color:var(--muted)">Listening together &bull; synced</div>
        <div class="bcard-icon">👥</div>
        <h3>Listen Together</h3>
        <p>Real-time synced sessions. Share a code and listen with friends anywhere.</p>
      </div>

      <!-- Stats card -->
      <div class="bcard bc5">
        <div class="mini-chart" id="miniChart">
          <div class="mc-bar" style="height:30%"></div>
          <div class="mc-bar" style="height:55%"></div>
          <div class="mc-bar" style="height:40%"></div>
          <div class="mc-bar" style="height:75%"></div>
          <div class="mc-bar" style="height:60%"></div>
          <div class="mc-bar" style="height:90%"></div>
          <div class="mc-bar" style="height:70%"></div>
        </div>
        <div class="bcard-icon">📊</div>
        <h3>Listening Stats</h3>
        <p>Personality insights, top artists, total hours, and your unique listening identity.</p>
      </div>

      <!-- Offline + Library -->
      <div class="bcard bc6" style="display:flex;gap:32px;flex-wrap:wrap;">
        <div style="flex:1;min-width:180px">
          <div class="bcard-icon">⬇️</div>
          <h3>Offline Cache</h3>
          <p>Download any song or playlist for uninterrupted listening without an internet connection.</p>
        </div>
        <div style="flex:1;min-width:180px">
          <div class="bcard-icon">📚</div>
          <h3>Smart Library</h3>
          <p>Songs, albums, artists, playlists, local media — all in one beautifully organized library.</p>
        </div>
      </div>

      <!-- Last.fm -->
      <div class="bcard bc7">
        <div class="bcard-icon">🎤</div>
        <h3>Last.fm Scrobble</h3>
        <p>Every track you play is automatically scrobbled to your Last.fm profile.</p>
      </div>

    </div>
  </section>

  <!-- FULL FEATURES LIST -->
  <section class="reveal">
    <span class="sec-eyebrow">Everything included</span>
    <h2 class="sec-title">No feature left behind.</h2>
    <p class="sec-sub">Sonora packs years of features that premium apps charge for, completely free.</p>

    <div class="feat-list">
      <div class="feat-item"><span class="feat-item-icon">🎶</span><div class="feat-item-text"><h4>YouTube Music Source</h4><p>Full catalogue streaming powered by InnerTube.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">📻</span><div class="feat-item-text"><h4>Radio &amp; Mix</h4><p>Artist, song, and genre-based radio stations.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">🔀</span><div class="feat-item-text"><h4>Queue &amp; Shuffle</h4><p>Smart queue with drag-to-reorder and sleep timer.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">🏠</span><div class="feat-item-text"><h4>Local Media</h4><p>Play music stored on your device seamlessly.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">🔁</span><div class="feat-item-text"><h4>Repeat Modes</h4><p>Off, repeat one, repeat all — always in control.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">🎮</span><div class="feat-item-text"><h4>Discord Rich Presence</h4><p>Show what you're listening to on Discord.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">🌙</span><div class="feat-item-text"><h4>Sleep Timer</h4><p>Auto-stop music after a set duration.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">📱</span><div class="feat-item-text"><h4>Material You</h4><p>Dynamic color theming that matches your wallpaper.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">🔍</span><div class="feat-item-text"><h4>Search &amp; Discover</h4><p>Find artists, albums, and songs instantly.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">⭐</span><div class="feat-item-text"><h4>Favorites</h4><p>Like and save songs to your local library.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">🔔</span><div class="feat-item-text"><h4>Media Notification</h4><p>Full playback controls from the notification shade.</p></div></div>
      <div class="feat-item"><span class="feat-item-icon">🛡️</span><div class="feat-item-text"><h4>Privacy First</h4><p>No analytics, no trackers. Your data stays yours.</p></div></div>
    </div>
  </section>

  <!-- CTA BANNER -->
  <div class="cta-banner reveal">
    <h2>Open Source &amp; Free Forever</h2>
    <p>Sonora is MIT licensed. Fork it, contribute, or just enjoy the music.</p>
    <div style="display:flex;justify-content:center;flex-wrap:wrap;gap:14px;">
      <a class="btn-grad" href="https://github.com/susil-kumar/Sonora" target="_blank" rel="noopener">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor"><path d="M12 2C6.477 2 2 6.484 2 12.017c0 4.425 2.865 8.18 6.839 9.504.5.092.682-.217.682-.483 0-.237-.008-.868-.013-1.703-2.782.605-3.369-1.343-3.369-1.343-.454-1.158-1.11-1.466-1.11-1.466-.908-.62.069-.608.069-.608 1.003.07 1.531 1.032 1.531 1.032.892 1.53 2.341 1.088 2.91.832.092-.647.35-1.088.636-1.338-2.22-.253-4.555-1.113-4.555-4.951 0-1.093.39-1.988 1.029-2.688-.103-.253-.446-1.272.098-2.65 0 0 .84-.27 2.75 1.026A9.564 9.564 0 0 1 12 6.844a9.59 9.59 0 0 1 2.504.337c1.909-1.296 2.747-1.027 2.747-1.027.546 1.379.202 2.398.1 2.651.64.7 1.028 1.595 1.028 2.688 0 3.848-2.339 4.695-4.566 4.943.359.309.678.92.678 1.855 0 1.338-.012 2.419-.012 2.747 0 .268.18.58.688.482A10.019 10.019 0 0 0 22 12.017C22 6.484 17.522 2 12 2z"/></svg>
        Star on GitHub
      </a>
      <a class="btn-outline" href="https://github.com/susil-kumar/Sonora/issues" target="_blank" rel="noopener">Report an Issue</a>
    </div>
  </div>

  <!-- SUPPORT -->
  <section style="padding-bottom:32px;" class="reveal">
    <div style="text-align:center;margin-bottom:40px;">
      <span class="sec-eyebrow">Support</span>
      <h2 class="sec-title">Fuel the dev.</h2>
    </div>
    <div class="support-card">
      <div class="support-emoji">☕</div>
      <h3>Buy Me a Coffee</h3>
      <p>Sonora is built with passion as a free, open-source project. If it brings you joy, consider supporting development via UPI.</p>
      <div class="upi-row">
        <span class="upi-val" id="upiId">iamsusil@fam</span>
        <button class="upi-copy" onclick="copyUPI()">Copy</button>
      </div>
      <p class="upi-hint">Open GPay, PhonePe, Paytm, or BHIM &rarr; Send money &rarr; paste the UPI ID. Thank you! 🙏</p>
    </div>
  </section>

  <!-- FOOTER -->
  <footer>
    <span>© 2026 Sonora &mdash; Made with ♥ by Susil Kumar</span>
    <div class="footer-links">
      <a href="https://github.com/susil-kumar/Sonora" target="_blank" rel="noopener">GitHub</a>
      <a href="/health">Server Health</a>
    </div>
  </footer>

</div>

<script>
  // UPI copy
  function copyUPI() {
    const id = document.getElementById('upiId').textContent;
    navigator.clipboard.writeText(id).then(() => {
      const btn = document.querySelector('.upi-copy');
      const orig = btn.textContent;
      btn.textContent = 'Copied!';
      btn.style.background = 'rgba(111,255,158,0.3)';
      setTimeout(() => { btn.textContent = orig; btn.style.background = ''; }, 1800);
    });
  }

  // Scroll reveal
  const ro = new IntersectionObserver((entries) => {
    entries.forEach((e, i) => {
      if (e.isIntersecting) {
        setTimeout(() => e.target.classList.add('shown'), i * 60);
        ro.unobserve(e.target);
      }
    });
  }, { threshold: 0.08 });
  document.querySelectorAll('.reveal, .bcard, .feat-item').forEach(el => {
    if (!el.classList.contains('reveal')) { el.classList.add('reveal'); }
    ro.observe(el);
  });

  // Mouse glow on bento cards
  document.querySelectorAll('.bcard').forEach(card => {
    card.addEventListener('mousemove', e => {
      const r = card.getBoundingClientRect();
      const x = ((e.clientX - r.left) / r.width * 100).toFixed(1) + '%';
      const y = ((e.clientY - r.top) / r.height * 100).toFixed(1) + '%';
      card.style.setProperty('--mx', x);
      card.style.setProperty('--my', y);
    });
  });

  // Animate mini chart bars
  const chartBars = document.querySelectorAll('.mc-bar');
  setInterval(() => {
    chartBars.forEach(b => {
      b.style.height = (20 + Math.random() * 75) + '%';
    });
  }, 2000);
</script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(html))
}


func (s *Server) handleIssueToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req struct {
		Scopes []string `json:"scopes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	scopeSet := map[string]bool{}
	for _, scope := range req.Scopes {
		normalized := strings.ToLower(strings.TrimSpace(scope))
		if normalized == "" {
			continue
		}
		if normalized == "together:rw" || normalized == "canvas:read" {
			scopeSet[normalized] = true
		}
	}
	if len(scopeSet) == 0 {
		scopeSet["together:rw"] = true
		scopeSet["canvas:read"] = true
	}
	scopes := make([]string, 0, len(scopeSet))
	for k := range scopeSet {
		scopes = append(scopes, k)
	}
	resp, err := issueToken(s.cfg, strings.Join(scopes, " "))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to issue token")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	if strings.TrimSpace(req.HostDisplayName) == "" {
		req.HostDisplayName = "Host"
	}
	room := s.store.create(req.Settings)
	wsURL := s.wsURLFromRequest(r)
	writeJSON(w, http.StatusOK, CreateSessionResponse{
		SessionID: room.SessionID,
		Code:      room.Code,
		HostKey:   room.HostKey,
		GuestKey:  room.GuestKey,
		WsURL:     wsURL,
		Settings:  room.Settings,
	})
}

func (s *Server) handleResolveSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	var req ResolveSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request")
		return
	}
	room := s.store.byCodeLookup(req.Code)
	if room == nil {
		writeError(w, http.StatusNotFound, "Session not found")
		return
	}
	writeJSON(w, http.StatusOK, ResolveSessionResponse{
		SessionID: room.SessionID,
		GuestKey:  room.GuestKey,
		WsURL:     s.wsURLFromRequest(r),
		Settings:  room.Settings,
	})
}

func (s *Server) wsURLFromRequest(r *http.Request) string {
	scheme := "ws"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "wss"
	}
	host := r.Host
	if xfHost := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); xfHost != "" {
		host = xfHost
	}
	return scheme + "://" + host + "/v1/together/ws"
}

func (s *Server) handleTogetherWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	var hello ClientHello
	if err := conn.ReadJSON(&hello); err != nil {
		_ = s.writeJSONWithTimeout(conn, ServerError{Type: "server_error", Message: "Invalid hello"})
		return
	}
	if hello.Type != "client_hello" {
		_ = s.writeJSONWithTimeout(conn, ServerError{Type: "server_error", SessionID: &hello.SessionID, Message: "Expected client_hello"})
		return
	}
	if hello.ProtocolVersion != 1 {
		_ = s.writeJSONWithTimeout(conn, ServerError{Type: "server_error", SessionID: &hello.SessionID, Message: "Unsupported protocol version"})
		return
	}

	room := s.store.bySessionLookup(hello.SessionID)
	if room == nil {
		_ = s.writeJSONWithTimeout(conn, ServerError{Type: "server_error", SessionID: &hello.SessionID, Message: "Session not found"})
		return
	}

	peer, welcome, err := s.registerPeer(room, conn, hello)
	if err != nil {
		_ = s.writeJSONWithTimeout(conn, ServerError{Type: "server_error", SessionID: &hello.SessionID, Message: err.Error()})
		return
	}

	if err := s.writeJSONWithTimeout(conn, welcome); err != nil {
		s.unregisterPeer(room, peer)
		return
	}

	if peer.IsHost {
		s.broadcastParticipantJoined(room, peer.Participant)
	} else {
		if room.Settings.RequireHostApprovalToJoin {
			s.notifyHostJoinRequest(room, peer.Participant)
		} else {
			peer.Approved = true
			s.broadcastParticipantJoined(room, peer.Participant)
		}
	}

	if state := room.currentRoomState(); state != nil && (peer.IsHost || peer.Approved || !room.Settings.RequireHostApprovalToJoin) {
		_ = s.writeJSONWithTimeout(conn, RoomStateMessage{Type: "room_state", State: *state})
	}

	for {
		var msg TogetherMessage
		if err := conn.ReadJSON(&msg); err != nil {
			s.unregisterPeer(room, peer)
			reason := "Disconnected"
			s.broadcastParticipantLeft(room, peer.Participant.ID, &reason)
			return
		}
		s.handlePeerMessage(room, peer, msg)
	}
}

func (s *Server) registerPeer(room *Room, conn *websocket.Conn, hello ClientHello) (*Peer, ServerWelcome, error) {
	room.mu.Lock()
	defer room.mu.Unlock()

	if room.banned[hello.ClientID] {
		return nil, ServerWelcome{}, errors.New("Banned")
	}

	if hello.SessionID != room.SessionID {
		return nil, ServerWelcome{}, errors.New("Session mismatch")
	}

	isHost := hello.SessionKey == room.HostKey
	if !isHost && hello.SessionKey != room.GuestKey {
		return nil, ServerWelcome{}, errors.New("Invalid session key")
	}

	participantID := randomHex(10)
	role := "GUEST"
	approved := !room.Settings.RequireHostApprovalToJoin
	if isHost {
		participantID = room.HostID
		role = "HOST"
		approved = true
	}

	name := strings.TrimSpace(hello.DisplayName)
	if name == "" {
		if isHost {
			name = "Host"
		} else {
			name = "Guest"
		}
	}

	participant := TogetherParticipant{
		ID:          participantID,
		Name:        name,
		IsHost:      isHost,
		IsPending:   !approved && !isHost,
		IsConnected: true,
	}
	peer := &Peer{
		Participant: participant,
		Conn:        conn,
		IsHost:      isHost,
		Approved:    approved,
		ClientID:    strings.TrimSpace(hello.ClientID),
		SessionKey:  hello.SessionKey,
	}

	if isHost {
		if room.hostPeer != nil && room.hostPeer.Conn != nil {
			sid := room.SessionID
			_ = s.writeJSONWithTimeout(room.hostPeer.Conn, ServerError{Type: "server_error", SessionID: &sid, Message: "Host replaced"})
			_ = room.hostPeer.Conn.Close()
		}
		room.hostPeer = peer
	}
	room.participants[participant.ID] = peer

	welcome := ServerWelcome{
		Type:            "server_welcome",
		ProtocolVersion: 1,
		SessionID:       room.SessionID,
		ParticipantID:   participant.ID,
		Role:            role,
		IsPending:       !peer.Approved && !peer.IsHost,
		Settings:        room.Settings,
	}
	return peer, welcome, nil
}

func (s *Server) unregisterPeer(room *Room, peer *Peer) {
	room.mu.Lock()
	defer room.mu.Unlock()
	delete(room.participants, peer.Participant.ID)
	if room.hostPeer == peer {
		room.hostPeer = nil
	}
}

func (s *Server) writeJSONWithTimeout(conn *websocket.Conn, payload any) error {
	if conn == nil {
		return errors.New("nil websocket connection")
	}
	deadline := time.Now().Add(s.cfg.WSWriteTimeout)
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	writeErr := conn.WriteJSON(payload)
	if err := conn.SetWriteDeadline(time.Time{}); err != nil && writeErr == nil {
		writeErr = err
	}
	return writeErr
}

func (s *Server) handlePeerMessage(room *Room, peer *Peer, msg TogetherMessage) {
	switch msg.Type {
	case "heartbeat_ping":
		var ping HeartbeatPing
		if err := json.Unmarshal(msg.Raw, &ping); err != nil {
			return
		}
		_ = s.writeJSONWithTimeout(peer.Conn, HeartbeatPong{
			Type:                  "heartbeat_pong",
			SessionID:             room.SessionID,
			PingID:                ping.PingID,
			ClientElapsedRealtime: ping.ClientElapsedRealtime,
			ServerElapsedRealtime: elapsedRealtimeMs(),
		})
	case "room_state":
		if !peer.IsHost {
			s.sendCodeError(peer.Conn, room.SessionID, "Only host can broadcast room state", "NOT_HOST")
			return
		}
		var state RoomStateMessage
		if err := json.Unmarshal(msg.Raw, &state); err != nil {
			return
		}
		state.State.SessionID = room.SessionID
		room.applyRoomStateFromHost(state.State)
		s.broadcastToApproved(room, state, nil)
	case "control_request":
		var req ControlRequest
		if err := json.Unmarshal(msg.Raw, &req); err != nil {
			return
		}
		if !peer.IsHost && !room.Settings.AllowGuestsToControlPlayback {
			s.broadcastServerIssueToGuest(peer, room.SessionID, "Guests cannot control playback", "GUEST_CONTROL_DISABLED")
			return
		}
		host := s.currentHost(room)
		if host == nil {
			s.broadcastServerIssueToGuest(peer, room.SessionID, "Host is offline", "HOST_OFFLINE")
			return
		}
		if !peer.IsHost {
			_ = s.writeJSONWithTimeout(host.Conn, req)
		}
	case "add_track_request":
		var req AddTrackRequest
		if err := json.Unmarshal(msg.Raw, &req); err != nil {
			return
		}
		if !peer.IsHost && !room.Settings.AllowGuestsToAddTracks {
			s.broadcastServerIssueToGuest(peer, room.SessionID, "Guests cannot add tracks", "GUEST_ADD_DISABLED")
			return
		}
		host := s.currentHost(room)
		if host == nil {
			s.broadcastServerIssueToGuest(peer, room.SessionID, "Host is offline", "HOST_OFFLINE")
			return
		}
		if !peer.IsHost {
			_ = s.writeJSONWithTimeout(host.Conn, req)
		}
	case "join_decision":
		if !peer.IsHost {
			return
		}
		var decision JoinDecisionMessage
		if err := json.Unmarshal(msg.Raw, &decision); err != nil {
			return
		}
		target := s.findPeer(room, decision.ParticipantID)
		if target == nil {
			return
		}
		if !decision.Approved {
			_ = s.writeJSONWithTimeout(target.Conn, decision)
			_ = target.Conn.Close()
			s.unregisterPeer(room, target)
			reason := "Rejected"
			s.broadcastParticipantLeft(room, target.Participant.ID, &reason)
			return
		}
		target.Approved = true
		target.Participant.IsPending = false
		_ = s.writeJSONWithTimeout(target.Conn, decision)
		s.broadcastParticipantJoined(room, target.Participant)
		if state := room.currentRoomState(); state != nil {
			_ = s.writeJSONWithTimeout(target.Conn, RoomStateMessage{Type: "room_state", State: *state})
		}
	case "kick":
		if !peer.IsHost {
			return
		}
		var kick KickMessage
		if err := json.Unmarshal(msg.Raw, &kick); err != nil {
			return
		}
		target := s.findPeer(room, kick.ParticipantID)
		if target == nil {
			return
		}
		_ = s.writeJSONWithTimeout(target.Conn, kick)
		_ = target.Conn.Close()
		s.unregisterPeer(room, target)
		s.broadcastParticipantLeft(room, target.Participant.ID, kick.Reason)
	case "ban":
		if !peer.IsHost {
			return
		}
		var ban BanMessage
		if err := json.Unmarshal(msg.Raw, &ban); err != nil {
			return
		}
		target := s.findPeer(room, ban.ParticipantID)
		if target == nil {
			return
		}
		room.mu.Lock()
		if target.ClientID != "" {
			room.banned[target.ClientID] = true
		}
		room.mu.Unlock()
		_ = s.writeJSONWithTimeout(target.Conn, ban)
		_ = target.Conn.Close()
		s.unregisterPeer(room, target)
		s.broadcastParticipantLeft(room, target.Participant.ID, ban.Reason)
	case "client_leave":
		_ = peer.Conn.Close()
	}
}

func (s *Server) currentHost(room *Room) *Peer {
	room.mu.RLock()
	defer room.mu.RUnlock()
	if room.hostPeer != nil && room.hostPeer.Conn != nil {
		return room.hostPeer
	}
	return nil
}

func (s *Server) findPeer(room *Room, participantID string) *Peer {
	room.mu.RLock()
	defer room.mu.RUnlock()
	return room.participants[participantID]
}

func (s *Server) sendCodeError(conn *websocket.Conn, sessionID, message, code string) {
	codeCopy := code
	sid := sessionID
	_ = s.writeJSONWithTimeout(conn, ServerError{Type: "server_error", SessionID: &sid, Message: message, Code: &codeCopy})
}

func (s *Server) notifyHostJoinRequest(room *Room, participant TogetherParticipant) {
	host := s.currentHost(room)
	if host == nil {
		return
	}
	_ = s.writeJSONWithTimeout(host.Conn, JoinRequestMessage{
		Type:        "join_request",
		SessionID:   room.SessionID,
		Participant: participant,
	})
}

func (s *Server) broadcastParticipantJoined(room *Room, participant TogetherParticipant) {
	msg := ParticipantJoinedMessage{
		Type:        "participant_joined",
		SessionID:   room.SessionID,
		Participant: participant,
	}
	s.broadcastToApproved(room, msg, nil)
}

func (s *Server) broadcastParticipantLeft(room *Room, participantID string, reason *string) {
	msg := ParticipantLeftMessage{
		Type:          "participant_left",
		SessionID:     room.SessionID,
		ParticipantID: participantID,
		Reason:        reason,
	}
	s.broadcastToApproved(room, msg, nil)
}

func (s *Server) broadcastToApproved(room *Room, payload any, exclude *Peer) {
	room.mu.RLock()
	peers := make([]*Peer, 0, len(room.participants))
	for _, p := range room.participants {
		if exclude != nil && p == exclude {
			continue
		}
		if p.IsHost || p.Approved || !room.Settings.RequireHostApprovalToJoin {
			peers = append(peers, p)
		}
	}
	room.mu.RUnlock()

	slowPeers := make([]*Peer, 0)
	for _, p := range peers {
		if p.Conn == nil {
			continue
		}
		if err := s.writeJSONWithTimeout(p.Conn, payload); err != nil {
			slowPeers = append(slowPeers, p)
		}
	}
	if len(slowPeers) == 0 {
		return
	}

	for _, p := range slowPeers {
		reason := fmt.Sprintf("slow peer dropped: %v", p.Participant.ID)
		_ = p.Conn.Close()
		s.unregisterPeer(room, p)
		s.broadcastParticipantLeft(room, p.Participant.ID, &reason)
	}
}

func (s *Server) handleCanvasHealth(w http.ResponseWriter, r *http.Request) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.cfg.CanvasUpstreamBase+"/health", nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create request")
		return
	}
	if token := strings.TrimSpace(s.cfg.CanvasUpstreamToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Canvas upstream unavailable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) handleCanvasProxy(w http.ResponseWriter, r *http.Request) {
	upstreamURL, err := url.Parse(s.cfg.CanvasUpstreamBase)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Invalid canvas upstream base URL")
		return
	}
	upstreamURL.Path = "/"
	q := upstreamURL.Query()
	for key, values := range r.URL.Query() {
		for _, v := range values {
			q.Add(key, v)
		}
	}
	upstreamURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL.String(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create request")
		return
	}
	if token := strings.TrimSpace(s.cfg.CanvasUpstreamToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Canvas upstream unavailable")
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) broadcastServerIssueToGuest(peer *Peer, sessionID, message, code string) {
	sid := sessionID
	codeCopy := code
	_ = s.writeJSONWithTimeout(peer.Conn, ServerError{Type: "server_error", SessionID: &sid, Message: message, Code: &codeCopy})
}

func main() {
	cfg := loadConfig()
	srv := newServer(cfg)
	addr := ":" + cfg.Port
	log.Printf("Sonora server listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Fatal(err)
	}
}
