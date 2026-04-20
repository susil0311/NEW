package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	Port               string
	JWTSigningKey      string
	TokenIssuer        string
	TokenTTL           time.Duration
	CanvasUpstreamBase string
	CanvasUpstreamToken string
}

func loadConfig() Config {
	cfg := Config{
		Port:               envOrDefault("PORT", "8080"),
		JWTSigningKey:      envOrDefault("JWT_SIGNING_KEY", "dev-only-change-me"),
		TokenIssuer:        envOrDefault("TOKEN_ISSUER", "sonora-self-hosted"),
		TokenTTL:           parseMinutes(envOrDefault("TOKEN_TTL_MINUTES", "60")),
		CanvasUpstreamBase: strings.TrimRight(envOrDefault("CANVAS_UPSTREAM_BASE_URL", "https://artwork-sonora.koiiverse.cloud"), "/"),
		CanvasUpstreamToken: strings.TrimSpace(os.Getenv("CANVAS_UPSTREAM_TOKEN")),
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
		_ = conn.WriteJSON(ServerError{Type: "server_error", Message: "Invalid hello"})
		return
	}
	if hello.Type != "client_hello" {
		_ = conn.WriteJSON(ServerError{Type: "server_error", SessionID: &hello.SessionID, Message: "Expected client_hello"})
		return
	}
	if hello.ProtocolVersion != 1 {
		_ = conn.WriteJSON(ServerError{Type: "server_error", SessionID: &hello.SessionID, Message: "Unsupported protocol version"})
		return
	}

	room := s.store.bySessionLookup(hello.SessionID)
	if room == nil {
		_ = conn.WriteJSON(ServerError{Type: "server_error", SessionID: &hello.SessionID, Message: "Session not found"})
		return
	}

	peer, welcome, err := s.registerPeer(room, conn, hello)
	if err != nil {
		_ = conn.WriteJSON(ServerError{Type: "server_error", SessionID: &hello.SessionID, Message: err.Error()})
		return
	}

	if err := conn.WriteJSON(welcome); err != nil {
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
		_ = conn.WriteJSON(RoomStateMessage{Type: "room_state", State: *state})
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
			_ = room.hostPeer.Conn.WriteJSON(ServerError{Type: "server_error", SessionID: &sid, Message: "Host replaced"})
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

func (s *Server) handlePeerMessage(room *Room, peer *Peer, msg TogetherMessage) {
	switch msg.Type {
	case "heartbeat_ping":
		var ping HeartbeatPing
		if err := json.Unmarshal(msg.Raw, &ping); err != nil {
			return
		}
		_ = peer.Conn.WriteJSON(HeartbeatPong{
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
			_ = host.Conn.WriteJSON(req)
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
			_ = host.Conn.WriteJSON(req)
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
			_ = target.Conn.WriteJSON(decision)
			_ = target.Conn.Close()
			s.unregisterPeer(room, target)
			reason := "Rejected"
			s.broadcastParticipantLeft(room, target.Participant.ID, &reason)
			return
		}
		target.Approved = true
		target.Participant.IsPending = false
		_ = target.Conn.WriteJSON(decision)
		s.broadcastParticipantJoined(room, target.Participant)
		if state := room.currentRoomState(); state != nil {
			_ = target.Conn.WriteJSON(RoomStateMessage{Type: "room_state", State: *state})
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
		_ = target.Conn.WriteJSON(kick)
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
		_ = target.Conn.WriteJSON(ban)
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
	_ = conn.WriteJSON(ServerError{Type: "server_error", SessionID: &sid, Message: message, Code: &codeCopy})
}

func (s *Server) notifyHostJoinRequest(room *Room, participant TogetherParticipant) {
	host := s.currentHost(room)
	if host == nil {
		return
	}
	_ = host.Conn.WriteJSON(JoinRequestMessage{
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
	for _, p := range peers {
		if p.Conn == nil {
			continue
		}
		_ = p.Conn.WriteJSON(payload)
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
	_ = peer.Conn.WriteJSON(ServerError{Type: "server_error", SessionID: &sid, Message: message, Code: &codeCopy})
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
