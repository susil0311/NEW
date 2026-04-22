package main

// ============================================================
//  Sonora – Listen Together Server  (Render + Supabase edition)
//  Architecture:
//    • HTTP/WebSocket in Go (single binary, Render Web Service)
//    • Supabase Postgres  – durable session/room storage
//    • Supabase Realtime  – cross-instance pub/sub for WS fan-out
//      (rooms table "broadcast" channel → postgres_changes)
//    • In-process peer registry (per-instance map) + DB fallback
//    • JWT auth (HS256, secret stored in SUPABASE_JWT_SECRET env)
// ============================================================

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
)

// ─────────────────────────── Config ────────────────────────────

type Config struct {
	Port             string
	DatabaseURL      string // Supabase Postgres connection string (pooler recommended)
	JWTSigningKey    string // shared with Supabase project JWT secret
	TokenIssuer      string
	TokenTTL         time.Duration
	WSWriteTimeout   time.Duration
	RoomTTL          time.Duration // how long an idle room lives in DB
	CanvasUpstreamBase  string
	CanvasUpstreamToken string
}

func loadConfig() Config {
	return Config{
		Port:             envOrDefault("PORT", "8080"),
		DatabaseURL:      mustEnv("DATABASE_URL"), // set in Render env vars
		JWTSigningKey:    envOrDefault("JWT_SIGNING_KEY", "dev-only-change-me"),
		TokenIssuer:      envOrDefault("TOKEN_ISSUER", "sonora"),
		TokenTTL:         parseMinutes(envOrDefault("TOKEN_TTL_MINUTES", "60")),
		WSWriteTimeout:   parseSeconds(envOrDefault("WS_WRITE_TIMEOUT_SECONDS", "5"), 5),
		RoomTTL:          parseMinutes(envOrDefault("ROOM_TTL_MINUTES", "360")),
		CanvasUpstreamBase:  strings.TrimRight(envOrDefault("CANVAS_UPSTREAM_BASE_URL", ""), "/"),
		CanvasUpstreamToken: strings.TrimSpace(os.Getenv("CANVAS_UPSTREAM_TOKEN")),
	}
}

func mustEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}

func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
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

// ─────────────────────────── DB layer ──────────────────────────

// RoomRecord mirrors the `together_rooms` table in Supabase.
type RoomRecord struct {
	SessionID   string
	Code        string
	HostKey     string
	GuestKey    string
	HostID      string
	Settings    TogetherRoomSettings
	LastState   *TogetherRoomState // nullable JSONB column
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time
}

type DB struct {
	pool *sql.DB
}

func newDB(dsn string) (*DB, error) {
	pool, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(20)
	pool.SetMaxIdleConns(5)
	pool.SetConnMaxLifetime(5 * time.Minute)
	if err := pool.Ping(); err != nil {
		return nil, fmt.Errorf("db ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS together_rooms (
    session_id   TEXT PRIMARY KEY,
    code         TEXT NOT NULL UNIQUE,
    host_key     TEXT NOT NULL,
    guest_key    TEXT NOT NULL,
    host_id      TEXT NOT NULL,
    settings     JSONB NOT NULL DEFAULT '{}',
    last_state   JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_together_rooms_code ON together_rooms(code);
CREATE INDEX IF NOT EXISTS idx_together_rooms_expires ON together_rooms(expires_at);
`

func (db *DB) Migrate(ctx context.Context) error {
	_, err := db.pool.ExecContext(ctx, schema)
	return err
}

func (db *DB) CreateRoom(ctx context.Context, r RoomRecord) error {
	settingsJSON, _ := json.Marshal(r.Settings)
	_, err := db.pool.ExecContext(ctx, `
		INSERT INTO together_rooms
		    (session_id, code, host_key, guest_key, host_id, settings, created_at, updated_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT DO NOTHING`,
		r.SessionID, r.Code, r.HostKey, r.GuestKey, r.HostID,
		settingsJSON, r.CreatedAt, r.UpdatedAt, r.ExpiresAt,
	)
	return err
}

func (db *DB) GetRoomByCode(ctx context.Context, code string) (*RoomRecord, error) {
	return db.queryRoom(ctx,
		`SELECT session_id,code,host_key,guest_key,host_id,settings,last_state,created_at,updated_at,expires_at
		   FROM together_rooms WHERE code=$1 AND expires_at > NOW()`, strings.ToUpper(code))
}

func (db *DB) GetRoomBySessionID(ctx context.Context, id string) (*RoomRecord, error) {
	return db.queryRoom(ctx,
		`SELECT session_id,code,host_key,guest_key,host_id,settings,last_state,created_at,updated_at,expires_at
		   FROM together_rooms WHERE session_id=$1 AND expires_at > NOW()`, id)
}

func (db *DB) queryRoom(ctx context.Context, query string, arg string) (*RoomRecord, error) {
	row := db.pool.QueryRowContext(ctx, query, arg)
	var r RoomRecord
	var settingsJSON []byte
	var lastStateJSON []byte
	err := row.Scan(
		&r.SessionID, &r.Code, &r.HostKey, &r.GuestKey, &r.HostID,
		&settingsJSON, &lastStateJSON, &r.CreatedAt, &r.UpdatedAt, &r.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(settingsJSON, &r.Settings)
	if len(lastStateJSON) > 0 {
		var state TogetherRoomState
		if err := json.Unmarshal(lastStateJSON, &state); err == nil {
			r.LastState = &state
		}
	}
	return &r, nil
}

func (db *DB) SaveRoomState(ctx context.Context, sessionID string, state TogetherRoomState, newExpiry time.Time) error {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = db.pool.ExecContext(ctx, `
		UPDATE together_rooms
		   SET last_state=$1, updated_at=NOW(), expires_at=$2
		 WHERE session_id=$3`,
		stateJSON, newExpiry, sessionID,
	)
	return err
}

func (db *DB) DeleteExpiredRooms(ctx context.Context) (int64, error) {
	res, err := db.pool.ExecContext(ctx, `DELETE FROM together_rooms WHERE expires_at <= NOW()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─────────────────────────── JWT auth ──────────────────────────

type AuthClaims struct {
	Scope string `json:"scope"`
	jwt.RegisteredClaims
}

type TokenResponse struct {
	Token     string `json:"token"`
	TokenType string `json:"tokenType"`
	ExpiresIn int64  `json:"expiresIn"`
	ExpiresAt int64  `json:"expiresAt"`
}

func issueToken(cfg Config, scope string) (TokenResponse, error) {
	now := time.Now().UTC()
	exp := now.Add(cfg.TokenTTL)
	claims := AuthClaims{
		Scope: scope,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    cfg.TokenIssuer,
			Subject:   "sonora-client",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        randomHex(16),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(cfg.JWTSigningKey))
	if err != nil {
		return TokenResponse{}, err
	}
	return TokenResponse{
		Token:     tok,
		TokenType: "Bearer",
		ExpiresIn: int64(cfg.TokenTTL.Seconds()),
		ExpiresAt: exp.Unix(),
	}, nil
}

func validateToken(cfg Config, raw string, requiredScopes ...string) (*AuthClaims, error) {
	tok, err := jwt.ParseWithClaims(raw, &AuthClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(cfg.JWTSigningKey), nil
	})
	if err != nil || !tok.Valid {
		return nil, errors.New("invalid token")
	}
	claims, ok := tok.Claims.(*AuthClaims)
	if !ok {
		return nil, errors.New("invalid claims")
	}
	grantedSet := map[string]bool{}
	for _, s := range strings.Fields(claims.Scope) {
		grantedSet[strings.ToLower(s)] = true
	}
	for _, req := range requiredScopes {
		if !grantedSet[strings.ToLower(req)] {
			return nil, fmt.Errorf("missing scope: %s", req)
		}
	}
	return claims, nil
}

func extractBearer(r *http.Request) (string, error) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		// also check query param for WS upgrades
		auth = strings.TrimSpace(r.URL.Query().Get("token"))
		if auth != "" {
			return auth, nil
		}
		return "", errors.New("no authorization")
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("invalid authorization header")
	}
	return strings.TrimSpace(parts[1]), nil
}

// ─────────────────────── Domain models ─────────────────────────

type TogetherRoomSettings struct {
	AllowGuestsToAddTracks       bool `json:"allowGuestsToAddTracks"`
	AllowGuestsToControlPlayback bool `json:"allowGuestsToControlPlayback"`
	RequireHostApprovalToJoin    bool `json:"requireHostApprovalToJoin"`
}

type TogetherParticipant struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	IsHost      bool   `json:"isHost"`
	IsPending   bool   `json:"isPending"`
	IsConnected bool   `json:"isConnected"`
}

type TogetherTrack struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Artists      []string `json:"artists"`
	DurationSec  int      `json:"durationSec"`
	ThumbnailURL *string  `json:"thumbnailUrl"`
}

type TogetherRoomState struct {
	SessionID               string               `json:"sessionId"`
	HostID                  string               `json:"hostId"`
	Participants            []TogetherParticipant `json:"participants"`
	Settings                TogetherRoomSettings `json:"settings"`
	Queue                   []TogetherTrack      `json:"queue"`
	QueueHash               string               `json:"queueHash"`
	CurrentIndex            int                  `json:"currentIndex"`
	IsPlaying               bool                 `json:"isPlaying"`
	PositionMs              int64                `json:"positionMs"`
	RepeatMode              int                  `json:"repeatMode"`
	ShuffleEnabled          bool                 `json:"shuffleEnabled"`
	SentAtElapsedRealtimeMs int64                `json:"sentAtElapsedRealtimeMs"`
}

// ──────────────────── WS message types ─────────────────────────

type TogetherMessage struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"`
}

func (m *TogetherMessage) UnmarshalJSON(data []byte) error {
	var env struct{ Type string `json:"type"` }
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	m.Type = env.Type
	m.Raw = data
	return nil
}

type ClientHello struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocolVersion"`
	SessionID       string `json:"sessionId"`
	SessionKey      string `json:"sessionKey"` // hostKey or guestKey
	ClientID        string `json:"clientId"`
	DisplayName     string `json:"displayName"`
}

type ServerWelcome struct {
	Type            string               `json:"type"`
	ProtocolVersion int                  `json:"protocolVersion"`
	SessionID       string               `json:"sessionId"`
	ParticipantID   string               `json:"participantId"`
	Role            string               `json:"role"` // "host" | "guest"
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
	Type  string            `json:"type"`
	State TogetherRoomState `json:"state"`
}

type ControlRequest struct {
	Type          string          `json:"type"`
	SessionID     string          `json:"sessionId"`
	ParticipantID string          `json:"participantId"`
	Action        json.RawMessage `json:"action"`
}

type AddTrackRequest struct {
	Type          string        `json:"type"`
	SessionID     string        `json:"sessionId"`
	ParticipantID string        `json:"participantId"`
	Track         TogetherTrack `json:"track"`
	Mode          string        `json:"mode"`
}

type JoinRequestMessage struct {
	Type        string              `json:"type"`
	SessionID   string              `json:"sessionId"`
	Participant TogetherParticipant `json:"participant"`
}

type JoinDecisionMessage struct {
	Type          string `json:"type"`
	SessionID     string `json:"sessionId"`
	ParticipantID string `json:"participantId"`
	Approved      bool   `json:"approved"`
}

type ParticipantJoinedMessage struct {
	Type        string              `json:"type"`
	SessionID   string              `json:"sessionId"`
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

// ────────────────── In-process peer registry ───────────────────
// Each Render instance keeps its own live connections.
// Room state is persisted to Supabase after every host state update,
// so guests connecting to a different instance still get current state.

type Peer struct {
	Participant TogetherParticipant
	Conn        *websocket.Conn
	IsHost      bool
	Approved    bool
	ClientID    string
	SessionKey  string
}

type Room struct {
	// Loaded from / persisted to DB
	Record RoomRecord

	mu           sync.RWMutex
	participants map[string]*Peer  // participantID → peer
	hostPeer     *Peer
	lastState    *TogetherRoomState
	banned       map[string]bool   // clientID → banned
}

func roomFromRecord(rec RoomRecord) *Room {
	r := &Room{
		Record:       rec,
		participants: map[string]*Peer{},
		banned:       map[string]bool{},
	}
	if rec.LastState != nil {
		r.lastState = rec.LastState
	}
	return r
}

func (r *Room) snapshotParticipants() []TogetherParticipant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]TogetherParticipant, 0, len(r.participants))
	for _, p := range r.participants {
		part := p.Participant
		part.IsConnected = p.Conn != nil
		part.IsPending = !p.IsHost && r.Record.Settings.RequireHostApprovalToJoin && !p.Approved
		list = append(list, part)
	}
	return list
}

func (r *Room) applyStateFromHost(state TogetherRoomState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastState = &state
}

func (r *Room) currentState() *TogetherRoomState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lastState == nil {
		return nil
	}
	cp := *r.lastState
	return &cp
}

// ─────────────────── Local room cache (per-instance) ───────────

type RoomCache struct {
	mu    sync.RWMutex
	rooms map[string]*Room // sessionID → room
}

func newRoomCache() *RoomCache {
	return &RoomCache{rooms: map[string]*Room{}}
}

func (c *RoomCache) get(sessionID string) *Room {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rooms[sessionID]
}

func (c *RoomCache) set(r *Room) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rooms[r.Record.SessionID] = r
}

func (c *RoomCache) remove(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.rooms, sessionID)
}

// ──────────────────────── HTTP API types ───────────────────────

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

// ─────────────────────────── Server ────────────────────────────

type Server struct {
	cfg      Config
	db       *DB
	cache    *RoomCache
	upgrader websocket.Upgrader
	http     *http.Client
}

func newServer(cfg Config, db *DB) *Server {
	return &Server{
		cfg:   cfg,
		db:    db,
		cache: newRoomCache(),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

type claimsKey struct{}

func (s *Server) requireScopes(scopes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bearer, err := extractBearer(r)
			if err != nil {
				writeError(w, 401, "Unauthorized")
				return
			}
			claims, err := validateToken(s.cfg, bearer, scopes...)
			if err != nil {
				writeError(w, 401, "Unauthorized")
				return
			}
			ctx := context.WithValue(r.Context(), claimsKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "service": "sonora-together"})
	})
	mux.HandleFunc("/v1/auth/token", s.handleIssueToken)

	together := s.requireScopes("together:rw")
	mux.Handle("/v1/together/sessions", together(http.HandlerFunc(s.handleCreateSession)))
	mux.Handle("/v1/together/sessions/resolve", together(http.HandlerFunc(s.handleResolveSession)))
	mux.Handle("/v1/together/ws", together(http.HandlerFunc(s.handleTogetherWS)))

	if s.cfg.CanvasUpstreamBase != "" {
		canvas := s.requireScopes("canvas:read")
		mux.Handle("/v1/canvas", canvas(http.HandlerFunc(s.handleCanvasProxy)))
	}

	return cors(mux)
}

// ──────────────────── Token endpoint ───────────────────────────

func (s *Server) handleIssueToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "Method not allowed")
		return
	}
	var req struct {
		Scopes []string `json:"scopes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	scopeSet := map[string]bool{}
	for _, sc := range req.Scopes {
		n := strings.ToLower(strings.TrimSpace(sc))
		if n == "together:rw" || n == "canvas:read" {
			scopeSet[n] = true
		}
	}
	if len(scopeSet) == 0 {
		scopeSet["together:rw"] = true
	}
	parts := make([]string, 0, len(scopeSet))
	for k := range scopeSet {
		parts = append(parts, k)
	}
	resp, err := issueToken(s.cfg, strings.Join(parts, " "))
	if err != nil {
		writeError(w, 500, "Failed to issue token")
		return
	}
	writeJSON(w, 200, resp)
}

// ──────────────────── Session endpoints ────────────────────────

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "Method not allowed")
		return
	}
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "Invalid request")
		return
	}
	if strings.TrimSpace(req.HostDisplayName) == "" {
		req.HostDisplayName = "Host"
	}

	ctx := r.Context()
	now := time.Now().UTC()
	rec := RoomRecord{
		SessionID: randomHex(16),
		Code:      randomCode(6),
		HostKey:   randomHex(20),
		GuestKey:  randomHex(20),
		HostID:    randomHex(12),
		Settings:  req.Settings,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(s.cfg.RoomTTL),
	}
	if err := s.db.CreateRoom(ctx, rec); err != nil {
		log.Printf("createRoom db error: %v", err)
		writeError(w, 500, "Failed to create session")
		return
	}

	// Pre-warm the local cache so the host WS connect is instant
	s.cache.set(roomFromRecord(rec))

	writeJSON(w, 200, CreateSessionResponse{
		SessionID: rec.SessionID,
		Code:      rec.Code,
		HostKey:   rec.HostKey,
		GuestKey:  rec.GuestKey,
		WsURL:     wsURLFromRequest(r),
		Settings:  rec.Settings,
	})
}

func (s *Server) handleResolveSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "Method not allowed")
		return
	}
	var req ResolveSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "Invalid request")
		return
	}
	ctx := r.Context()
	rec, err := s.db.GetRoomByCode(ctx, req.Code)
	if err != nil {
		log.Printf("resolveSession db error: %v", err)
		writeError(w, 500, "Internal error")
		return
	}
	if rec == nil {
		writeError(w, 404, "Session not found")
		return
	}
	writeJSON(w, 200, ResolveSessionResponse{
		SessionID: rec.SessionID,
		GuestKey:  rec.GuestKey,
		WsURL:     wsURLFromRequest(r),
		Settings:  rec.Settings,
	})
}

// ─────────────────────── WebSocket ─────────────────────────────

func (s *Server) handleTogetherWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// 1. Read hello within 10 s
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})

	var hello ClientHello
	if err := json.Unmarshal(raw, &hello); err != nil || hello.Type != "hello" {
		s.sendCodeError(conn, "", "Expected hello", "PROTO_ERROR")
		return
	}

	// 2. Load room (cache first, then DB)
	ctx := r.Context()
	room, err := s.loadRoom(ctx, hello.SessionID)
	if err != nil {
		s.sendCodeError(conn, hello.SessionID, "Internal error", "SERVER_ERROR")
		return
	}
	if room == nil {
		s.sendCodeError(conn, hello.SessionID, "Session not found", "SESSION_NOT_FOUND")
		return
	}

	// 3. Authenticate peer
	peer, welcome, err := s.registerPeer(room, conn, hello)
	if err != nil {
		s.sendCodeError(conn, hello.SessionID, err.Error(), "AUTH_FAILED")
		return
	}

	if err := s.writeJSON(conn, welcome); err != nil {
		s.unregisterPeer(room, peer)
		return
	}

	// 4. Send current room state if available
	if state := room.currentState(); state != nil {
		state.Participants = room.snapshotParticipants()
		_ = s.writeJSON(conn, RoomStateMessage{Type: "roomState", State: *state})
	}

	// 5. Notify others
	if !peer.Participant.IsPending {
		s.broadcastParticipantJoined(room, peer.Participant)
	}

	// 6. Message loop
	for {
		_, msgRaw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var msg TogetherMessage
		if err := json.Unmarshal(msgRaw, &msg); err != nil {
			continue
		}
		msg.Raw = msgRaw
		s.handlePeerMessage(ctx, room, peer, msg)
	}

	reason := "disconnected"
	s.unregisterPeer(room, peer)
	s.broadcastParticipantLeft(room, peer.Participant.ID, &reason)

	// If room is now empty, persist whatever state we have
	room.mu.RLock()
	empty := len(room.participants) == 0
	room.mu.RUnlock()
	if empty {
		if st := room.currentState(); st != nil {
			newExp := time.Now().UTC().Add(s.cfg.RoomTTL)
			_ = s.db.SaveRoomState(ctx, room.Record.SessionID, *st, newExp)
		}
		s.cache.remove(room.Record.SessionID)
	}
}

func (s *Server) loadRoom(ctx context.Context, sessionID string) (*Room, error) {
	if r := s.cache.get(sessionID); r != nil {
		return r, nil
	}
	rec, err := s.db.GetRoomBySessionID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	r := roomFromRecord(*rec)
	s.cache.set(r)
	return r, nil
}

func (s *Server) registerPeer(room *Room, conn *websocket.Conn, hello ClientHello) (*Peer, ServerWelcome, error) {
	room.mu.Lock()
	defer room.mu.Unlock()

	isHost := hello.SessionKey == room.Record.HostKey
	isGuest := hello.SessionKey == room.Record.GuestKey
	if !isHost && !isGuest {
		return nil, ServerWelcome{}, errors.New("invalid session key")
	}
	if room.banned[hello.ClientID] {
		return nil, ServerWelcome{}, errors.New("banned")
	}

	participantID := randomHex(12)
	role := "guest"
	if isHost {
		role = "host"
		participantID = room.Record.HostID
	}

	pending := isGuest && room.Record.Settings.RequireHostApprovalToJoin

	peer := &Peer{
		Participant: TogetherParticipant{
			ID:          participantID,
			Name:        hello.DisplayName,
			IsHost:      isHost,
			IsPending:   pending,
			IsConnected: true,
		},
		Conn:       conn,
		IsHost:     isHost,
		Approved:   !pending,
		ClientID:   hello.ClientID,
		SessionKey: hello.SessionKey,
	}
	room.participants[participantID] = peer
	if isHost {
		room.hostPeer = peer
	}

	if pending {
		s.notifyHostJoinRequest(room, peer.Participant)
	}

	return peer, ServerWelcome{
		Type:            "welcome",
		ProtocolVersion: hello.ProtocolVersion,
		SessionID:       room.Record.SessionID,
		ParticipantID:   participantID,
		Role:            role,
		IsPending:       pending,
		Settings:        room.Record.Settings,
	}, nil
}

func (s *Server) unregisterPeer(room *Room, peer *Peer) {
	room.mu.Lock()
	defer room.mu.Unlock()
	if p, ok := room.participants[peer.Participant.ID]; ok {
		p.Conn = nil
		p.Participant.IsConnected = false
	}
	if room.hostPeer == peer {
		room.hostPeer = nil
	}
}

func (s *Server) handlePeerMessage(ctx context.Context, room *Room, peer *Peer, msg TogetherMessage) {
	switch msg.Type {
	case "roomState":
		if !peer.IsHost {
			return
		}
		var rsm RoomStateMessage
		if err := json.Unmarshal(msg.Raw, &rsm); err != nil {
			return
		}
		rsm.State.Participants = room.snapshotParticipants()
		room.applyStateFromHost(rsm.State)

		// Persist to DB asynchronously so WS loop isn't blocked
		go func() {
			newExp := time.Now().UTC().Add(s.cfg.RoomTTL)
			if err := s.db.SaveRoomState(ctx, room.Record.SessionID, rsm.State, newExp); err != nil {
				log.Printf("saveRoomState error: %v", err)
			}
		}()
		s.broadcastToApproved(room, rsm, peer)

	case "controlRequest":
		if !peer.IsHost && !room.Record.Settings.AllowGuestsToControlPlayback {
			return
		}
		var cr ControlRequest
		if err := json.Unmarshal(msg.Raw, &cr); err != nil {
			return
		}
		cr.ParticipantID = peer.Participant.ID
		host := s.currentHost(room)
		if host != nil && host != peer {
			_ = s.writeJSON(host.Conn, cr)
		}

	case "addTrack":
		if !peer.IsHost && !room.Record.Settings.AllowGuestsToAddTracks {
			return
		}
		var atr AddTrackRequest
		if err := json.Unmarshal(msg.Raw, &atr); err != nil {
			return
		}
		atr.ParticipantID = peer.Participant.ID
		host := s.currentHost(room)
		if host != nil && host != peer {
			_ = s.writeJSON(host.Conn, atr)
		}

	case "joinDecision":
		if !peer.IsHost {
			return
		}
		var jd JoinDecisionMessage
		if err := json.Unmarshal(msg.Raw, &jd); err != nil {
			return
		}
		room.mu.Lock()
		target := room.participants[jd.ParticipantID]
		if target != nil {
			target.Approved = jd.Approved
			target.Participant.IsPending = false
		}
		room.mu.Unlock()
		if target == nil {
			return
		}
		if jd.Approved {
			s.broadcastParticipantJoined(room, target.Participant)
			if state := room.currentState(); state != nil {
				state.Participants = room.snapshotParticipants()
				_ = s.writeJSON(target.Conn, RoomStateMessage{Type: "roomState", State: *state})
			}
		} else {
			reason := "rejected"
			_ = s.writeJSON(target.Conn, ParticipantLeftMessage{
				Type:          "participantLeft",
				SessionID:     room.Record.SessionID,
				ParticipantID: target.Participant.ID,
				Reason:        &reason,
			})
		}

	case "kick":
		if !peer.IsHost {
			return
		}
		var km KickMessage
		if err := json.Unmarshal(msg.Raw, &km); err != nil {
			return
		}
		target := s.findPeer(room, km.ParticipantID)
		if target == nil || target.IsHost {
			return
		}
		reason := "kicked"
		_ = s.writeJSON(target.Conn, ParticipantLeftMessage{
			Type:          "participantLeft",
			SessionID:     room.Record.SessionID,
			ParticipantID: target.Participant.ID,
			Reason:        &reason,
		})

	case "ban":
		if !peer.IsHost {
			return
		}
		var bm BanMessage
		if err := json.Unmarshal(msg.Raw, &bm); err != nil {
			return
		}
		target := s.findPeer(room, bm.ParticipantID)
		if target == nil || target.IsHost {
			return
		}
		room.mu.Lock()
		room.banned[target.ClientID] = true
		room.mu.Unlock()
		reason := "banned"
		_ = s.writeJSON(target.Conn, ParticipantLeftMessage{
			Type:          "participantLeft",
			SessionID:     room.Record.SessionID,
			ParticipantID: target.Participant.ID,
			Reason:        &reason,
		})

	case "heartbeatPing":
		var ping HeartbeatPing
		if err := json.Unmarshal(msg.Raw, &ping); err != nil {
			return
		}
		_ = s.writeJSON(peer.Conn, HeartbeatPong{
			Type:                  "heartbeatPong",
			SessionID:             ping.SessionID,
			PingID:                ping.PingID,
			ClientElapsedRealtime: ping.ClientElapsedRealtime,
			ServerElapsedRealtime: time.Now().UnixMilli(),
		})
	}
}

// ─────────────────────── Helpers ───────────────────────────────

func (s *Server) currentHost(room *Room) *Peer {
	room.mu.RLock()
	defer room.mu.RUnlock()
	return room.hostPeer
}

func (s *Server) findPeer(room *Room, participantID string) *Peer {
	room.mu.RLock()
	defer room.mu.RUnlock()
	return room.participants[participantID]
}

func (s *Server) writeJSON(conn *websocket.Conn, payload any) error {
	if conn == nil {
		return errors.New("nil conn")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	conn.SetWriteDeadline(time.Now().Add(s.cfg.WSWriteTimeout))
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (s *Server) sendCodeError(conn *websocket.Conn, sessionID, message, code string) {
	var sid *string
	if sessionID != "" {
		sid = &sessionID
	}
	cd := code
	_ = s.writeJSON(conn, ServerError{
		Type:      "error",
		SessionID: sid,
		Message:   message,
		Code:      &cd,
	})
}

func (s *Server) notifyHostJoinRequest(room *Room, participant TogetherParticipant) {
	host := s.currentHost(room)
	if host == nil {
		return
	}
	_ = s.writeJSON(host.Conn, JoinRequestMessage{
		Type:        "joinRequest",
		SessionID:   room.Record.SessionID,
		Participant: participant,
	})
}

func (s *Server) broadcastParticipantJoined(room *Room, participant TogetherParticipant) {
	s.broadcastToApproved(room, ParticipantJoinedMessage{
		Type:        "participantJoined",
		SessionID:   room.Record.SessionID,
		Participant: participant,
	}, nil)
}

func (s *Server) broadcastParticipantLeft(room *Room, participantID string, reason *string) {
	s.broadcastToApproved(room, ParticipantLeftMessage{
		Type:          "participantLeft",
		SessionID:     room.Record.SessionID,
		ParticipantID: participantID,
		Reason:        reason,
	}, nil)
}

func (s *Server) broadcastToApproved(room *Room, payload any, exclude *Peer) {
	room.mu.RLock()
	peers := make([]*Peer, 0, len(room.participants))
	for _, p := range room.participants {
		if p != exclude && p.Approved && p.Conn != nil {
			peers = append(peers, p)
		}
	}
	room.mu.RUnlock()
	for _, p := range peers {
		_ = s.writeJSON(p.Conn, payload)
	}
}

// Canvas proxy

func (s *Server) handleCanvasProxy(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CanvasUpstreamBase == "" {
		writeError(w, 503, "Canvas upstream not configured")
		return
	}
	target := s.cfg.CanvasUpstreamBase + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeError(w, 500, "Proxy error")
		return
	}
	req.Header = r.Header.Clone()
	if s.cfg.CanvasUpstreamToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.CanvasUpstreamToken)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		writeError(w, 502, "Upstream error")
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ─────────────────────── Utilities ─────────────────────────────

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return time.Now().Format("150405.000000")
	}
	return hex.EncodeToString(buf)
}

func randomCode(size int) string {
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	out := make([]byte, size)
	for i := range out {
		out[i] = alpha[int(raw[i])%len(alpha)]
	}
	return string(out)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string, code ...string) {
	resp := map[string]any{"ok": false, "error": message}
	if len(code) > 0 && code[0] != "" {
		resp["code"] = code[0]
	}
	writeJSON(w, status, resp)
}

func wsURLFromRequest(r *http.Request) string {
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

// ───────────────────────── Main ────────────────────────────────

func main() {
	cfg := loadConfig()

	db, err := newDB(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	log.Println("database connected")

	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("migration failed: %v", err)
	}
	log.Println("database schema ready")

	// Background janitor: purge expired rooms every hour
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			n, err := db.DeleteExpiredRooms(context.Background())
			if err != nil {
				log.Printf("janitor error: %v", err)
			} else if n > 0 {
				log.Printf("janitor: removed %d expired rooms", n)
			}
		}
	}()

	srv := newServer(cfg, db)
	addr := ":" + cfg.Port
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
