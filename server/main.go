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
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width,initial-scale=1" />
  <title>Sonora — Self-Hosted Server</title>
  <style>
    @import url('https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700;800&display=swap');

    :root {
      --bg:      #080d1a;
      --bg2:     #0d1428;
      --surface: rgba(255,255,255,0.055);
      --surface2: rgba(255,255,255,0.03);
      --text:    #e4ecff;
      --muted:   #8899cc;
      --accent:  #6b8eff;
      --accent2: #56d4f5;
      --green:   #3de88e;
      --border:  rgba(255,255,255,0.10);
      --border2: rgba(255,255,255,0.06);
      --glow:    rgba(107,142,255,0.18);
    }

    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

    html { scroll-behavior: smooth; }

    body {
      font-family: 'Inter', system-ui, -apple-system, sans-serif;
      background: var(--bg);
      color: var(--text);
      min-height: 100vh;
      overflow-x: hidden;
    }

    /* ── Animated background ── */
    .bg-canvas {
      position: fixed;
      inset: 0;
      z-index: 0;
      pointer-events: none;
      overflow: hidden;
    }
    .bg-canvas::before {
      content: '';
      position: absolute;
      width: 900px; height: 900px;
      border-radius: 50%;
      background: radial-gradient(circle, rgba(107,142,255,0.12) 0%, transparent 70%);
      top: -300px; left: -200px;
      animation: drift1 18s ease-in-out infinite alternate;
    }
    .bg-canvas::after {
      content: '';
      position: absolute;
      width: 700px; height: 700px;
      border-radius: 50%;
      background: radial-gradient(circle, rgba(86,212,245,0.10) 0%, transparent 70%);
      bottom: -200px; right: -100px;
      animation: drift2 22s ease-in-out infinite alternate;
    }
    .bg-orb3 {
      position: absolute;
      width: 500px; height: 500px;
      border-radius: 50%;
      background: radial-gradient(circle, rgba(61,232,142,0.07) 0%, transparent 70%);
      top: 40%; left: 55%;
      animation: drift3 28s ease-in-out infinite alternate;
    }
    @keyframes drift1 { from { transform: translate(0,0) scale(1); } to { transform: translate(80px,60px) scale(1.15); } }
    @keyframes drift2 { from { transform: translate(0,0) scale(1); } to { transform: translate(-60px,-80px) scale(1.2); } }
    @keyframes drift3 { from { transform: translate(0,0) scale(1); } to { transform: translate(-40px,50px) scale(0.9); } }

    /* ── Floating music bars (decorative) ── */
    .bars {
      position: absolute;
      bottom: 60px; right: 60px;
      display: flex; align-items: flex-end; gap: 5px;
      opacity: 0.12;
    }
    .bar {
      width: 6px; border-radius: 3px;
      background: linear-gradient(to top, var(--accent), var(--accent2));
      animation: bounce var(--d, 1.2s) ease-in-out infinite alternate;
    }
    @keyframes bounce { from { height: 10px; } to { height: var(--h, 40px); } }

    /* ── Layout ── */
    .page {
      position: relative; z-index: 1;
      max-width: 1020px;
      margin: 0 auto;
      padding: 48px 24px 80px;
    }

    /* ── Nav bar ── */
    nav {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 72px;
      animation: fadeDown 0.6s ease both;
    }
    .nav-logo {
      display: flex; align-items: center; gap: 10px;
      font-weight: 700; font-size: 18px; letter-spacing: -.02em;
    }
    .nav-logo .dot {
      width: 28px; height: 28px; border-radius: 8px;
      background: linear-gradient(135deg, var(--accent), var(--accent2));
      display: grid; place-items: center;
      box-shadow: 0 0 18px rgba(107,142,255,0.5);
      animation: pulse 3s ease-in-out infinite;
    }
    @keyframes pulse { 0%,100% { box-shadow: 0 0 18px rgba(107,142,255,0.5); } 50% { box-shadow: 0 0 32px rgba(107,142,255,0.85); } }
    .nav-pill {
      font-size: 12px; font-weight: 500;
      padding: 5px 12px; border-radius: 999px;
      border: 1px solid var(--border);
      color: var(--muted);
      background: var(--surface2);
    }
    .status-dot {
      display: inline-block;
      width: 7px; height: 7px; border-radius: 50%;
      background: var(--green);
      margin-right: 6px;
      box-shadow: 0 0 8px var(--green);
      animation: blink 2s ease-in-out infinite;
    }
    @keyframes blink { 0%,100% { opacity:1; } 50% { opacity:.4; } }

    /* ── Hero ── */
    .hero {
      margin-bottom: 64px;
      animation: fadeUp 0.7s 0.1s ease both;
    }
    .eyebrow {
      display: inline-flex; align-items: center; gap: 8px;
      font-size: 12px; font-weight: 600; letter-spacing: .12em;
      text-transform: uppercase;
      color: var(--accent);
      margin-bottom: 20px;
    }
    .eyebrow-line {
      width: 24px; height: 1px; background: var(--accent); opacity: .6;
    }
    .hero h1 {
      font-size: clamp(36px, 5.5vw, 64px);
      font-weight: 800;
      line-height: 1.06;
      letter-spacing: -.03em;
      margin-bottom: 20px;
    }
    .hero h1 .grad {
      background: linear-gradient(100deg, var(--accent) 0%, var(--accent2) 55%, var(--green) 100%);
      -webkit-background-clip: text;
      -webkit-text-fill-color: transparent;
      background-clip: text;
    }
    .hero p {
      font-size: 17px;
      line-height: 1.65;
      color: var(--muted);
      max-width: 620px;
      margin-bottom: 32px;
    }
    .hero-btns {
      display: flex; flex-wrap: wrap; gap: 12px;
    }
    .btn-primary {
      display: inline-flex; align-items: center; gap: 8px;
      text-decoration: none;
      font-size: 14px; font-weight: 600;
      padding: 12px 22px; border-radius: 12px;
      background: linear-gradient(135deg, var(--accent), #8ba8ff);
      color: #fff;
      box-shadow: 0 4px 24px rgba(107,142,255,0.35);
      transition: transform .2s, box-shadow .2s;
    }
    .btn-primary:hover { transform: translateY(-2px); box-shadow: 0 8px 32px rgba(107,142,255,0.5); }
    .btn-ghost {
      display: inline-flex; align-items: center; gap: 8px;
      text-decoration: none;
      font-size: 14px; font-weight: 500;
      padding: 12px 22px; border-radius: 12px;
      border: 1px solid var(--border);
      color: var(--text);
      background: var(--surface2);
      transition: border-color .2s, background .2s;
    }
    .btn-ghost:hover { border-color: rgba(255,255,255,.25); background: var(--surface); }

    /* ── Section label ── */
    .section-label {
      font-size: 11px; font-weight: 700; letter-spacing: .14em;
      text-transform: uppercase; color: var(--muted);
      margin-bottom: 20px;
    }

    /* ── Feature grid ── */
    .features {
      margin-bottom: 56px;
      animation: fadeUp 0.7s 0.2s ease both;
    }
    .feature-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(210px, 1fr));
      gap: 14px;
    }
    .feature-card {
      border: 1px solid var(--border2);
      border-radius: 18px;
      padding: 22px 20px;
      background: var(--surface);
      backdrop-filter: blur(10px);
      transition: border-color .25s, transform .25s, box-shadow .25s;
      cursor: default;
    }
    .feature-card:hover {
      border-color: rgba(107,142,255,.35);
      transform: translateY(-4px);
      box-shadow: 0 12px 40px rgba(107,142,255,.15);
    }
    .feature-icon {
      width: 42px; height: 42px; border-radius: 12px;
      display: grid; place-items: center;
      font-size: 20px;
      margin-bottom: 14px;
      background: linear-gradient(135deg, rgba(107,142,255,.2), rgba(86,212,245,.1));
      border: 1px solid rgba(107,142,255,.2);
    }
    .feature-card h3 {
      font-size: 15px; font-weight: 700;
      margin-bottom: 8px; color: var(--text);
    }
    .feature-card p {
      font-size: 13px; line-height: 1.6;
      color: var(--muted);
    }

    /* ── API endpoints ── */
    .endpoints {
      margin-bottom: 56px;
      animation: fadeUp 0.7s 0.3s ease both;
    }
    .endpoint-list {
      display: flex; flex-direction: column; gap: 8px;
    }
    .endpoint {
      display: flex; align-items: center; gap: 14px;
      padding: 14px 18px; border-radius: 14px;
      border: 1px solid var(--border2);
      background: var(--surface2);
      transition: border-color .2s, background .2s;
      text-decoration: none; color: inherit;
    }
    .endpoint:hover { border-color: var(--border); background: var(--surface); }
    .method {
      font-size: 11px; font-weight: 700; letter-spacing: .06em;
      padding: 3px 8px; border-radius: 6px; min-width: 44px;
      text-align: center;
    }
    .get  { background: rgba(61,232,142,.15);  color: var(--green); }
    .post { background: rgba(107,142,255,.15); color: var(--accent); }
    .ws   { background: rgba(86,212,245,.15);  color: var(--accent2); }
    .endpoint-path {
      font-size: 13px; font-family: 'SF Mono', 'Fira Code', monospace;
      color: var(--text); font-weight: 500;
      flex: 1;
    }
    .endpoint-desc {
      font-size: 12px; color: var(--muted);
    }

    /* ── About / meta ── */
    .about {
      animation: fadeUp 0.7s 0.4s ease both;
    }
    .about-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 14px;
    }
    @media (max-width: 640px) { .about-grid { grid-template-columns: 1fr; } }

    .about-card {
      border: 1px solid var(--border2);
      border-radius: 18px;
      padding: 24px;
      background: var(--surface);
      backdrop-filter: blur(10px);
    }
    .about-card.full { grid-column: 1 / -1; }
    .about-card h3 {
      font-size: 14px; font-weight: 700;
      color: var(--muted); letter-spacing: .05em;
      text-transform: uppercase; margin-bottom: 16px;
    }

    .meta-row {
      display: flex; justify-content: space-between;
      align-items: center;
      padding: 10px 0;
      border-bottom: 1px solid var(--border2);
      font-size: 14px;
    }
    .meta-row:last-child { border-bottom: none; }
    .meta-row .label { color: var(--muted); }
    .meta-row .value { font-weight: 500; color: var(--text); text-align: right; }
    .meta-row .value code {
      font-family: 'SF Mono', 'Fira Code', monospace;
      font-size: 12px;
      background: rgba(255,255,255,.07);
      padding: 2px 7px; border-radius: 5px;
    }

    /* ── Sonora app features ── */
    .app-features {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
      gap: 10px;
    }
    .app-feat {
      display: flex; align-items: center; gap: 8px;
      font-size: 13px; color: var(--muted);
      padding: 8px 12px; border-radius: 10px;
      background: rgba(255,255,255,.04);
      border: 1px solid var(--border2);
    }
    .app-feat .ic { font-size: 15px; }

    /* ── UPI support ── */
    .upi-card {
      background: linear-gradient(135deg, rgba(61,232,142,.08), rgba(107,142,255,.08));
      border: 1px solid rgba(61,232,142,.2);
      border-radius: 18px;
      padding: 24px;
      grid-column: 1 / -1;
    }
    .upi-card h3 {
      color: var(--green) !important;
    }
    .upi-box {
      display: flex; align-items: center; gap: 12px;
      background: rgba(0,0,0,.25);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 14px 18px;
      margin-bottom: 12px;
    }
    .upi-id {
      font-family: 'SF Mono', 'Fira Code', monospace;
      font-size: 16px; font-weight: 600;
      color: var(--green);
      flex: 1;
      letter-spacing: .04em;
    }
    .copy-btn {
      font-size: 12px; font-weight: 600;
      padding: 6px 14px; border-radius: 8px;
      border: 1px solid rgba(61,232,142,.3);
      background: rgba(61,232,142,.12);
      color: var(--green);
      cursor: pointer; transition: background .2s;
      outline: none;
    }
    .copy-btn:hover { background: rgba(61,232,142,.22); }
    .upi-note {
      font-size: 13px; color: var(--muted); line-height: 1.5;
    }

    /* ── Footer ── */
    footer {
      margin-top: 72px;
      padding-top: 24px;
      border-top: 1px solid var(--border2);
      display: flex; flex-wrap: wrap;
      justify-content: space-between; align-items: center;
      gap: 12px;
      font-size: 13px; color: var(--muted);
      animation: fadeUp 0.7s 0.5s ease both;
    }
    footer a { color: var(--muted); text-decoration: none; }
    footer a:hover { color: var(--text); }
    .footer-links { display: flex; gap: 20px; flex-wrap: wrap; }

    /* ── Animations ── */
    @keyframes fadeUp   { from { opacity:0; transform:translateY(22px); } to { opacity:1; transform:none; } }
    @keyframes fadeDown { from { opacity:0; transform:translateY(-14px); } to { opacity:1; transform:none; } }
  </style>
</head>
<body>

<div class="bg-canvas">
  <div class="bg-orb3"></div>
  <div class="bars">
    <div class="bar" style="--h:24px;--d:1.1s"></div>
    <div class="bar" style="--h:42px;--d:0.8s"></div>
    <div class="bar" style="--h:34px;--d:1.4s"></div>
    <div class="bar" style="--h:52px;--d:0.9s"></div>
    <div class="bar" style="--h:28px;--d:1.2s"></div>
    <div class="bar" style="--h:46px;--d:1.6s"></div>
    <div class="bar" style="--h:20px;--d:1.0s"></div>
  </div>
</div>

<div class="page">

  <!-- Nav -->
  <nav>
    <div class="nav-logo">
      <div class="dot">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none">
          <path d="M9 18V5l12-2v13" stroke="#fff" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/>
          <circle cx="6" cy="18" r="3" stroke="#fff" stroke-width="2.2"/>
          <circle cx="18" cy="16" r="3" stroke="#fff" stroke-width="2.2"/>
        </svg>
      </div>
      Sonora
    </div>
    <span class="nav-pill"><span class="status-dot"></span>Server Online</span>
  </nav>

  <!-- Hero -->
  <section class="hero">
    <div class="eyebrow"><div class="eyebrow-line"></div>Self-Hosted Backend</div>
    <h1>Sync Music.<br><span class="grad">Together.</span></h1>
    <p>
      Sonora's self-hosted server powers real-time listening sessions, secure token issuance,
      and Canvas artwork proxy — all in a single Go binary you can deploy anywhere in seconds.
    </p>
    <div class="hero-btns">
      <a class="btn-primary" href="/health">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg>
        Health Check
      </a>
      <a class="btn-ghost" href="/v1/auth/token">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>
        Token Endpoint
      </a>
    </div>
  </section>

  <!-- Features -->
  <section class="features">
    <p class="section-label">What this server does</p>
    <div class="feature-grid">
      <div class="feature-card">
        <div class="feature-icon">🎵</div>
        <h3>Listen Together</h3>
        <p>Create live sessions with WebSocket sync. Join codes, host approval, guest controls, kick &amp; ban.</p>
      </div>
      <div class="feature-card">
        <div class="feature-icon">🔐</div>
        <h3>Runtime Tokens</h3>
        <p>Short-lived JWTs issued on demand. No static secrets shipped to the Android client.</p>
      </div>
      <div class="feature-card">
        <div class="feature-icon">🖼️</div>
        <h3>Canvas Proxy</h3>
        <p>Proxies album artwork and animated canvas from upstream — keeping credentials server-side.</p>
      </div>
      <div class="feature-card">
        <div class="feature-icon">⚡</div>
        <h3>Zero-dependency deploy</h3>
        <p>Single static binary. Works on Fly.io, Railway, Render, VPS — or your own machine.</p>
      </div>
      <div class="feature-card">
        <div class="feature-icon">🛡️</div>
        <h3>Privacy First</h3>
        <p>No analytics, no tracking. Your listening data stays between you and your guests.</p>
      </div>
      <div class="feature-card">
        <div class="feature-icon">🌐</div>
        <h3>CORS Ready</h3>
        <p>Full CORS support with structured JSON errors for easy integration from any client.</p>
      </div>
    </div>
  </section>

  <!-- API Endpoints -->
  <section class="endpoints">
    <p class="section-label">API endpoints</p>
    <div class="endpoint-list">
      <a class="endpoint" href="/health">
        <span class="method get">GET</span>
        <span class="endpoint-path">/health</span>
        <span class="endpoint-desc">Server health check</span>
      </a>
      <div class="endpoint">
        <span class="method post">POST</span>
        <span class="endpoint-path">/v1/auth/token</span>
        <span class="endpoint-desc">Issue scoped JWT token</span>
      </div>
      <div class="endpoint">
        <span class="method post">POST</span>
        <span class="endpoint-path">/v1/together/sessions</span>
        <span class="endpoint-desc">Create listening session • <code>together:rw</code></span>
      </div>
      <div class="endpoint">
        <span class="method post">POST</span>
        <span class="endpoint-path">/v1/together/sessions/resolve</span>
        <span class="endpoint-desc">Resolve join code → session • <code>together:rw</code></span>
      </div>
      <div class="endpoint">
        <span class="method ws">WS</span>
        <span class="endpoint-path">/v1/together/ws</span>
        <span class="endpoint-desc">Real-time sync WebSocket • <code>together:rw</code></span>
      </div>
      <div class="endpoint">
        <span class="method get">GET</span>
        <span class="endpoint-path">/v1/canvas</span>
        <span class="endpoint-desc">Canvas artwork proxy • <code>canvas:read</code></span>
      </div>
      <div class="endpoint">
        <span class="method get">GET</span>
        <span class="endpoint-path">/v1/canvas/health</span>
        <span class="endpoint-desc">Canvas upstream health • <code>canvas:read</code></span>
      </div>
    </div>
  </section>

  <!-- About -->
  <section class="about">
    <p class="section-label">About Sonora</p>
    <div class="about-grid">

      <!-- Project info -->
      <div class="about-card">
        <h3>Project Details</h3>
        <div class="meta-row"><span class="label">Developer</span><span class="value">Susil Kumar</span></div>
        <div class="meta-row"><span class="label">App</span><span class="value">Sonora Music</span></div>
        <div class="meta-row"><span class="label">Platform</span><span class="value">Android</span></div>
        <div class="meta-row"><span class="label">Health endpoint</span><span class="value"><code>/health</code></span></div>
        <div class="meta-row"><span class="label">Token endpoint</span><span class="value"><code>POST /v1/auth/token</code></span></div>
      </div>

      <!-- App features -->
      <div class="about-card">
        <h3>Sonora App Features</h3>
        <div class="app-features">
          <div class="app-feat"><span class="ic">🎶</span>YouTube Music</div>
          <div class="app-feat"><span class="ic">👥</span>Listen Together</div>
          <div class="app-feat"><span class="ic">🎨</span>Canvas Art</div>
          <div class="app-feat"><span class="ic">📝</span>Lyrics</div>
          <div class="app-feat"><span class="ic">📻</span>Radio</div>
          <div class="app-feat"><span class="ic">🎛️</span>Equalizer</div>
          <div class="app-feat"><span class="ic">⬇️</span>Offline Cache</div>
          <div class="app-feat"><span class="ic">🔀</span>Smart Queue</div>
          <div class="app-feat"><span class="ic">🌙</span>Sleep Timer</div>
          <div class="app-feat"><span class="ic">🏠</span>Local Media</div>
          <div class="app-feat"><span class="ic">🎤</span>Last.fm Scrobble</div>
          <div class="app-feat"><span class="ic">🎮</span>Discord RPC</div>
        </div>
      </div>

      <!-- UPI Support -->
      <div class="about-card upi-card">
        <h3>☕ Support Development</h3>
        <p class="upi-note" style="margin-bottom:16px">
          Sonora is built with ❤️ as a free, open-source project. If it brings you joy,
          consider buying the developer a coffee — every contribution keeps the project alive!
        </p>
        <div class="upi-box">
          <span class="upi-id" id="upiId">iamsusil@fam</span>
          <button class="copy-btn" onclick="copyUPI()">Copy</button>
        </div>
        <p class="upi-note">
          Open any UPI app (GPay, PhonePe, Paytm, BHIM) → Send money → paste the ID above.
          No minimum. Thank you for supporting independent development! 🙏
        </p>
      </div>

    </div>
  </section>

  <!-- Footer -->
  <footer>
    <span>© 2026 Sonora · Made with ♥ by Susil Kumar</span>
    <div class="footer-links">
      <a href="/health">Health</a>
      <a href="/v1/auth/token">Token API</a>
    </div>
  </footer>

</div>

<script>
  function copyUPI() {
    const id = document.getElementById('upiId').textContent;
    navigator.clipboard.writeText(id).then(() => {
      const btn = document.querySelector('.copy-btn');
      const orig = btn.textContent;
      btn.textContent = 'Copied!';
      btn.style.background = 'rgba(61,232,142,.35)';
      setTimeout(() => { btn.textContent = orig; btn.style.background = ''; }, 1800);
    });
  }

  // Stagger feature card entrance
  const observer = new IntersectionObserver((entries) => {
    entries.forEach((e, i) => {
      if (e.isIntersecting) {
        e.target.style.animation = 'fadeUp 0.5s ' + (i * 0.07) + 's ease both';
        observer.unobserve(e.target);
      }
    });
  }, { threshold: 0.1 });
  document.querySelectorAll('.feature-card, .endpoint, .about-card').forEach(el => {
    el.style.opacity = '0';
    observer.observe(el);
  });
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
