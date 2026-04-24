package main

// ============================================================
//  Sonora – Listen Together Server  (Render + Supabase edition)
//  github.com/koiverse/Sonora
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
	Port                string
	DatabaseURL         string
	JWTSigningKey       string
	TokenIssuer         string
	TokenTTL            time.Duration
	WSWriteTimeout      time.Duration
	RoomTTL             time.Duration
	CanvasUpstreamBase  string
	CanvasUpstreamToken string
}

func loadConfig() Config {
	return Config{
		Port:                envOrDefault("PORT", "8080"),
		DatabaseURL:         mustEnv("DATABASE_URL"),
		JWTSigningKey:       envOrDefault("JWT_SIGNING_KEY", "dev-only-change-me"),
		TokenIssuer:         envOrDefault("TOKEN_ISSUER", "sonora"),
		TokenTTL:            parseMinutes(envOrDefault("TOKEN_TTL_MINUTES", "60")),
		WSWriteTimeout:      parseSeconds(envOrDefault("WS_WRITE_TIMEOUT_SECONDS", "5"), 5),
		RoomTTL:             parseMinutes(envOrDefault("ROOM_TTL_MINUTES", "360")),
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

type RoomRecord struct {
	SessionID string
	Code      string
	HostKey   string
	GuestKey  string
	HostID    string
	Settings  TogetherRoomSettings
	LastState *TogetherRoomState
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time
}

type DB struct{ pool *sql.DB }

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
    session_id TEXT PRIMARY KEY,
    code       TEXT NOT NULL UNIQUE,
    host_key   TEXT NOT NULL,
    guest_key  TEXT NOT NULL,
    host_id    TEXT NOT NULL,
    settings   JSONB NOT NULL DEFAULT '{}',
    last_state JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_together_rooms_code    ON together_rooms(code);
CREATE INDEX IF NOT EXISTS idx_together_rooms_expires ON together_rooms(expires_at);
`

func (db *DB) Migrate(ctx context.Context) error {
	_, err := db.pool.ExecContext(ctx, schema)
	return err
}

func (db *DB) CreateRoom(ctx context.Context, r RoomRecord) error {
	settingsJSON, _ := json.Marshal(r.Settings)
	_, err := db.pool.ExecContext(ctx, `
		INSERT INTO together_rooms(session_id,code,host_key,guest_key,host_id,settings,created_at,updated_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`,
		r.SessionID, r.Code, r.HostKey, r.GuestKey, r.HostID,
		settingsJSON, r.CreatedAt, r.UpdatedAt, r.ExpiresAt)
	return err
}

func (db *DB) GetRoomByCode(ctx context.Context, code string) (*RoomRecord, error) {
	return db.queryRoom(ctx,
		`SELECT session_id,code,host_key,guest_key,host_id,settings,last_state,created_at,updated_at,expires_at
		   FROM together_rooms WHERE code=$1 AND expires_at>NOW()`, strings.ToUpper(code))
}

func (db *DB) GetRoomBySessionID(ctx context.Context, id string) (*RoomRecord, error) {
	return db.queryRoom(ctx,
		`SELECT session_id,code,host_key,guest_key,host_id,settings,last_state,created_at,updated_at,expires_at
		   FROM together_rooms WHERE session_id=$1 AND expires_at>NOW()`, id)
}

func (db *DB) queryRoom(ctx context.Context, query, arg string) (*RoomRecord, error) {
	var r RoomRecord
	var settingsJSON, lastStateJSON []byte
	err := db.pool.QueryRowContext(ctx, query, arg).Scan(
		&r.SessionID, &r.Code, &r.HostKey, &r.GuestKey, &r.HostID,
		&settingsJSON, &lastStateJSON,
		&r.CreatedAt, &r.UpdatedAt, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(settingsJSON, &r.Settings)
	if len(lastStateJSON) > 0 {
		var s TogetherRoomState
		if json.Unmarshal(lastStateJSON, &s) == nil {
			r.LastState = &s
		}
	}
	return &r, nil
}

func (db *DB) SaveRoomState(ctx context.Context, sessionID string, state TogetherRoomState, exp time.Time) error {
	j, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = db.pool.ExecContext(ctx,
		`UPDATE together_rooms SET last_state=$1,updated_at=NOW(),expires_at=$2 WHERE session_id=$3`,
		j, exp, sessionID)
	return err
}

func (db *DB) DeleteExpiredRooms(ctx context.Context) (int64, error) {
	res, err := db.pool.ExecContext(ctx, `DELETE FROM together_rooms WHERE expires_at<=NOW()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─────────────────────────── JWT ───────────────────────────────

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
	claims := AuthClaims{Scope: scope, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: cfg.TokenIssuer, Subject: "sonora-client",
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(exp),
		ID: randomHex(16),
	}}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(cfg.JWTSigningKey))
	if err != nil {
		return TokenResponse{}, err
	}
	return TokenResponse{tok, "Bearer", int64(cfg.TokenTTL.Seconds()), exp.Unix()}, nil
}

func validateToken(cfg Config, raw string, scopes ...string) (*AuthClaims, error) {
	tok, err := jwt.ParseWithClaims(raw, &AuthClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(cfg.JWTSigningKey), nil
	})
	if err != nil || !tok.Valid {
		return nil, errors.New("invalid token")
	}
	c, ok := tok.Claims.(*AuthClaims)
	if !ok {
		return nil, errors.New("invalid claims")
	}
	granted := map[string]bool{}
	for _, s := range strings.Fields(c.Scope) {
		granted[strings.ToLower(s)] = true
	}
	for _, req := range scopes {
		if !granted[strings.ToLower(req)] {
			return nil, fmt.Errorf("missing scope: %s", req)
		}
	}
	return c, nil
}

func extractBearer(r *http.Request) (string, error) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		if q := strings.TrimSpace(r.URL.Query().Get("token")); q != "" {
			return q, nil
		}
		return "", errors.New("no authorization")
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("bad authorization header")
	}
	return strings.TrimSpace(parts[1]), nil
}

// ─────────────────────── Domain models ─────────────────────────
// All json tags MUST match camelCase field names in Kotlin data classes.

// → Kotlin: TogetherRoomSettings
type TogetherRoomSettings struct {
	AllowGuestsToAddTracks       bool `json:"allowGuestsToAddTracks"`
	AllowGuestsToControlPlayback bool `json:"allowGuestsToControlPlayback"`
	RequireHostApprovalToJoin    bool `json:"requireHostApprovalToJoin"`
}

// → Kotlin: TogetherParticipant
type TogetherParticipant struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	IsHost      bool   `json:"isHost"`
	IsPending   bool   `json:"isPending"`
	IsConnected bool   `json:"isConnected"`
}

// → Kotlin: TogetherTrack
type TogetherTrack struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Artists      []string `json:"artists"`
	DurationSec  int      `json:"durationSec"`
	ThumbnailURL *string  `json:"thumbnailUrl"`
}

// → Kotlin: TogetherRoomState
type TogetherRoomState struct {
	SessionID               string                `json:"sessionId"`
	HostID                  string                `json:"hostId"`
	Participants            []TogetherParticipant `json:"participants"`
	Settings                TogetherRoomSettings  `json:"settings"`
	Queue                   []TogetherTrack       `json:"queue"`
	QueueHash               string                `json:"queueHash"`
	CurrentIndex            int                   `json:"currentIndex"`
	IsPlaying               bool                  `json:"isPlaying"`
	PositionMs              int64                 `json:"positionMs"`
	RepeatMode              int                   `json:"repeatMode"`
	ShuffleEnabled          bool                  `json:"shuffleEnabled"`
	SentAtElapsedRealtimeMs int64                 `json:"sentAtElapsedRealtimeMs"`
}

// ─────── Wire message structs – type strings MUST match ────────
// Kotlin classDiscriminator = "type" and each class's @SerialName

// Generic inbound envelope for type dispatch
type Inbound struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"`
}

func (m *Inbound) UnmarshalJSON(data []byte) error {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	m.Type = env.Type
	m.Raw = data
	return nil
}

// ── Inbound from Android ─────────────────────────────
// @SerialName("client_hello")
type ClientHello struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocolVersion"`
	SessionID       string `json:"sessionId"`
	SessionKey      string `json:"sessionKey"`
	ClientID        string `json:"clientId"`
	DisplayName     string `json:"displayName"`
}

// @SerialName("room_state") — host pushes state to server
type HostRoomState struct {
	Type  string            `json:"type"`
	State TogetherRoomState `json:"state"`
}

// @SerialName("control_request")
type ControlRequest struct {
	Type          string          `json:"type"`
	SessionID     string          `json:"sessionId"`
	ParticipantID string          `json:"participantId"`
	Action        json.RawMessage `json:"action"`
}

// @SerialName("add_track_request")
type AddTrackRequest struct {
	Type          string        `json:"type"`
	SessionID     string        `json:"sessionId"`
	ParticipantID string        `json:"participantId"`
	Track         TogetherTrack `json:"track"`
	Mode          string        `json:"mode"`
}

// @SerialName("join_decision")
type JoinDecision struct {
	Type          string `json:"type"`
	SessionID     string `json:"sessionId"`
	ParticipantID string `json:"participantId"`
	Approved      bool   `json:"approved"`
}

// @SerialName("kick")
type KickParticipant struct {
	Type          string  `json:"type"`
	SessionID     string  `json:"sessionId"`
	ParticipantID string  `json:"participantId"`
	Reason        *string `json:"reason,omitempty"`
}

// @SerialName("ban")
type BanParticipant struct {
	Type          string  `json:"type"`
	SessionID     string  `json:"sessionId"`
	ParticipantID string  `json:"participantId"`
	Reason        *string `json:"reason,omitempty"`
}

// @SerialName("heartbeat_ping")
type HeartbeatPing struct {
	Type                  string `json:"type"`
	SessionID             string `json:"sessionId"`
	PingID                int64  `json:"pingId"`
	ClientElapsedRealtime int64  `json:"clientElapsedRealtimeMs"`
}

// @SerialName("client_leave")
type ClientLeave struct {
	Type          string `json:"type"`
	SessionID     string `json:"sessionId"`
	ParticipantID string `json:"participantId"`
}

// ── Outbound to Android ──────────────────────────────
// @SerialName("server_welcome")
// role MUST be "HOST" or "GUEST" — matches Kotlin enum ServerRole { HOST, GUEST }
type ServerWelcome struct {
	Type            string               `json:"type"`
	ProtocolVersion int                  `json:"protocolVersion"`
	SessionID       string               `json:"sessionId"`
	ParticipantID   string               `json:"participantId"`
	Role            string               `json:"role"`
	IsPending       bool                 `json:"isPending"`
	Settings        TogetherRoomSettings `json:"settings"`
}

// @SerialName("server_error")
type ServerError struct {
	Type      string  `json:"type"`
	SessionID *string `json:"sessionId,omitempty"`
	Message   string  `json:"message"`
	Code      *string `json:"code,omitempty"`
}

// @SerialName("room_state") — server pushes to guests
type RoomStateMsg struct {
	Type  string            `json:"type"`
	State TogetherRoomState `json:"state"`
}

// @SerialName("join_request")
type JoinRequest struct {
	Type        string              `json:"type"`
	SessionID   string              `json:"sessionId"`
	Participant TogetherParticipant `json:"participant"`
}

// @SerialName("participant_joined")
type ParticipantJoined struct {
	Type        string              `json:"type"`
	SessionID   string              `json:"sessionId"`
	Participant TogetherParticipant `json:"participant"`
}

// @SerialName("participant_left")
type ParticipantLeft struct {
	Type          string  `json:"type"`
	SessionID     string  `json:"sessionId"`
	ParticipantID string  `json:"participantId"`
	Reason        *string `json:"reason,omitempty"`
}

// @SerialName("heartbeat_pong")
type HeartbeatPong struct {
	Type                  string `json:"type"`
	SessionID             string `json:"sessionId"`
	PingID                int64  `json:"pingId"`
	ClientElapsedRealtime int64  `json:"clientElapsedRealtimeMs"`
	ServerElapsedRealtime int64  `json:"serverElapsedRealtimeMs"`
}

// ─────────────────────── Peer / Room ───────────────────────────

type Peer struct {
	Participant TogetherParticipant
	Conn        *websocket.Conn
	IsHost      bool
	Approved    bool
	ClientID    string
}

type Room struct {
	Record       RoomRecord
	mu           sync.RWMutex
	participants map[string]*Peer
	hostPeer     *Peer
	lastState    *TogetherRoomState
	banned       map[string]bool
}

func newRoom(rec RoomRecord) *Room {
	r := &Room{Record: rec, participants: map[string]*Peer{}, banned: map[string]bool{}}
	if rec.LastState != nil {
		s := *rec.LastState
		r.lastState = &s
	}
	return r
}

func (r *Room) snapshotParticipants() []TogetherParticipant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]TogetherParticipant, 0, len(r.participants))
	for _, p := range r.participants {
		pt := p.Participant
		pt.IsConnected = p.Conn != nil
		pt.IsPending = !p.IsHost && r.Record.Settings.RequireHostApprovalToJoin && !p.Approved
		list = append(list, pt)
	}
	return list
}

func (r *Room) setState(s TogetherRoomState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastState = &s
}

func (r *Room) getState() *TogetherRoomState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lastState == nil {
		return nil
	}
	cp := *r.lastState
	return &cp
}

// ─────────────────────── Room cache ────────────────────────────

type RoomCache struct {
	mu    sync.RWMutex
	rooms map[string]*Room
}

func newRoomCache() *RoomCache             { return &RoomCache{rooms: map[string]*Room{}} }
func (c *RoomCache) get(id string) *Room   { c.mu.RLock(); defer c.mu.RUnlock(); return c.rooms[id] }
func (c *RoomCache) set(r *Room)           { c.mu.Lock(); defer c.mu.Unlock(); c.rooms[r.Record.SessionID] = r }
func (c *RoomCache) del(id string)         { c.mu.Lock(); defer c.mu.Unlock(); delete(c.rooms, id) }

// ─────────────────────── HTTP types ────────────────────────────

type CreateSessionReq struct {
	HostDisplayName string               `json:"hostDisplayName"`
	Settings        TogetherRoomSettings `json:"settings"`
}
type CreateSessionResp struct {
	SessionID string               `json:"sessionId"`
	Code      string               `json:"code"`
	HostKey   string               `json:"hostKey"`
	GuestKey  string               `json:"guestKey"`
	WsURL     string               `json:"wsUrl"`
	Settings  TogetherRoomSettings `json:"settings"`
}
type ResolveSessionReq  struct{ Code string `json:"code"` }
type ResolveSessionResp struct {
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
	client   *http.Client
}

func newServer(cfg Config, db *DB) *Server {
	return &Server{
		cfg:   cfg,
		db:    db,
		cache: newRoomCache(),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool { return true },
		},
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

type ctxKey struct{}

func (s *Server) auth(scopes ...string) func(http.Handler) http.Handler {
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
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, claims)))
		})
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.handleLanding)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "service": "sonora-together", "version": "2"})
	})
	mux.HandleFunc("/v1/auth/token", s.handleToken)

	rw := s.auth("together:rw")
	mux.Handle("/v1/together/sessions", rw(http.HandlerFunc(s.handleCreateSession)))
	mux.Handle("/v1/together/sessions/resolve", rw(http.HandlerFunc(s.handleResolveSession)))
	mux.Handle("/v1/together/ws", rw(http.HandlerFunc(s.handleWS)))

	if s.cfg.CanvasUpstreamBase != "" {
		mux.Handle("/v1/canvas", s.auth("canvas:read")(http.HandlerFunc(s.handleCanvas)))
	}
	return cors(mux)
}

// ─────────────────────── Landing page ──────────────────────────

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(landingHTML))
}

const landingHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>Sonora – Listen Together</title>
<style>
:root{--bg:#0a0a0f;--surface:#13131a;--border:#1e1e2e;--accent:#7c6af7;--accent2:#a78bfa;--text:#e2e2f0;--muted:#6b6b8a;--green:#22c55e}
*{box-sizing:border-box;margin:0;padding:0}
body{background:var(--bg);color:var(--text);font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;min-height:100vh;display:flex;flex-direction:column;align-items:center}
nav{width:100%;max-width:960px;display:flex;align-items:center;justify-content:space-between;padding:24px 32px}
.logo{display:flex;align-items:center;gap:10px;font-size:20px;font-weight:700;letter-spacing:-.3px;color:var(--text);text-decoration:none}
.logo-icon{width:36px;height:36px;background:linear-gradient(135deg,var(--accent),var(--accent2));border-radius:10px;display:flex;align-items:center;justify-content:center;font-size:18px}
.pill{background:rgba(124,106,247,.15);border:1px solid rgba(124,106,247,.3);color:var(--accent2);padding:4px 12px;border-radius:99px;font-size:12px;font-weight:600;letter-spacing:.5px;text-transform:uppercase}
.hero{width:100%;max-width:960px;padding:80px 32px 64px;text-align:center}
.hero-badge{display:inline-flex;align-items:center;gap:6px;background:rgba(124,106,247,.1);border:1px solid rgba(124,106,247,.25);color:var(--accent2);padding:6px 14px;border-radius:99px;font-size:13px;margin-bottom:32px}
.dot{width:6px;height:6px;background:var(--green);border-radius:50%;animation:pulse 2s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.3}}
h1{font-size:clamp(36px,6vw,64px);font-weight:800;line-height:1.1;letter-spacing:-1.5px;background:linear-gradient(135deg,#fff 0%,var(--accent2) 100%);-webkit-background-clip:text;-webkit-text-fill-color:transparent;background-clip:text;margin-bottom:20px}
.subtitle{font-size:18px;color:var(--muted);line-height:1.6;max-width:520px;margin:0 auto 40px}
.cta-row{display:flex;gap:12px;justify-content:center;flex-wrap:wrap}
.btn{display:inline-flex;align-items:center;gap:8px;padding:12px 24px;border-radius:12px;font-size:15px;font-weight:600;text-decoration:none;transition:all .2s}
.btn-primary{background:linear-gradient(135deg,var(--accent),#9f8bff);color:#fff;box-shadow:0 4px 20px rgba(124,106,247,.4)}
.btn-primary:hover{transform:translateY(-2px);box-shadow:0 6px 28px rgba(124,106,247,.5)}
.btn-secondary{background:var(--surface);border:1px solid var(--border);color:var(--text)}
.btn-secondary:hover{border-color:var(--accent)}
.waveform{display:flex;align-items:flex-end;justify-content:center;gap:4px;height:48px;margin:48px auto 0;width:fit-content}
.bar{width:4px;background:linear-gradient(to top,var(--accent),var(--accent2));border-radius:2px;animation:wave 1.4s ease-in-out infinite;opacity:.7}
.bar:nth-child(1){height:16px;animation-delay:0s}.bar:nth-child(2){height:28px;animation-delay:.1s}.bar:nth-child(3){height:40px;animation-delay:.2s}.bar:nth-child(4){height:32px;animation-delay:.3s}.bar:nth-child(5){height:48px;animation-delay:.4s}.bar:nth-child(6){height:36px;animation-delay:.5s}.bar:nth-child(7){height:48px;animation-delay:.4s}.bar:nth-child(8){height:32px;animation-delay:.3s}.bar:nth-child(9){height:40px;animation-delay:.2s}.bar:nth-child(10){height:28px;animation-delay:.1s}.bar:nth-child(11){height:16px;animation-delay:0s}
@keyframes wave{0%,100%{transform:scaleY(1)}50%{transform:scaleY(.4)}}
.features{width:100%;max-width:960px;padding:0 32px 80px;display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:16px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:16px;padding:24px;transition:border-color .2s}
.card:hover{border-color:rgba(124,106,247,.4)}
.card-icon{width:44px;height:44px;background:rgba(124,106,247,.12);border-radius:12px;display:flex;align-items:center;justify-content:center;font-size:20px;margin-bottom:16px}
.card h3{font-size:16px;font-weight:700;margin-bottom:8px}.card p{font-size:14px;color:var(--muted);line-height:1.5}
.status-section{width:100%;max-width:960px;padding:0 32px 80px}
.status-card{background:var(--surface);border:1px solid var(--border);border-radius:16px;padding:24px 28px;display:flex;align-items:flex-start;justify-content:space-between;flex-wrap:wrap;gap:24px}
.status-label{font-size:13px;color:var(--muted);margin-bottom:6px}
.status-value{font-size:15px;font-weight:600;display:flex;align-items:center;gap:8px}
.status-dot{width:8px;height:8px;background:var(--green);border-radius:50%;box-shadow:0 0 6px var(--green)}
.endpoints{display:flex;flex-direction:column;gap:8px}
.endpoint{display:flex;align-items:center;gap:10px;background:rgba(255,255,255,.04);border:1px solid var(--border);border-radius:10px;padding:10px 14px;font-size:13px;font-family:'SF Mono','Fira Code',monospace}
.method{font-size:11px;font-weight:700;padding:2px 8px;border-radius:6px;min-width:44px;text-align:center}
.method.post{background:rgba(34,197,94,.15);color:#4ade80}.method.get{background:rgba(59,130,246,.15);color:#60a5fa}.method.ws{background:rgba(124,106,247,.15);color:var(--accent2)}
.path-desc{color:var(--muted);margin-left:auto;font-size:12px;font-family:sans-serif}
footer{width:100%;border-top:1px solid var(--border);padding:24px 32px;display:flex;justify-content:center;align-items:center;gap:24px;flex-wrap:wrap;margin-top:auto}
footer a{color:var(--muted);font-size:13px;text-decoration:none}footer a:hover{color:var(--text)}
.footer-copy{color:var(--muted);font-size:13px}
</style>
</head>
<body>
<nav>
  <a class="logo" href="/"><div class="logo-icon">♪</div>Sonora</a>
  <span class="pill">Server v2</span>
</nav>
<section class="hero">
  <div class="hero-badge"><span class="dot"></span>Server online &amp; ready</div>
  <h1>Listen Together,<br/>Anywhere.</h1>
  <p class="subtitle">Real-time music sync server for the Sonora Android app. Share a 6-character code and listen in perfect sync with friends.</p>
  <div class="cta-row">
    <a class="btn btn-primary" href="https://github.com/koiverse/Sonora">★ View on GitHub</a>
    <a class="btn btn-secondary" href="/health">↗ Health Check</a>
  </div>
  <div class="waveform">
    <div class="bar"></div><div class="bar"></div><div class="bar"></div><div class="bar"></div><div class="bar"></div>
    <div class="bar"></div><div class="bar"></div><div class="bar"></div><div class="bar"></div><div class="bar"></div>
    <div class="bar"></div>
  </div>
</section>
<div class="features">
  <div class="card"><div class="card-icon">⚡</div><h3>Real-time Sync</h3><p>WebSocket-based playback state sync. Every seek, skip, and pause propagated instantly to all participants.</p></div>
  <div class="card"><div class="card-icon">🔒</div><h3>JWT Secured</h3><p>All API endpoints protected by short-lived HS256 tokens. Host and guest keys are cryptographically random.</p></div>
  <div class="card"><div class="card-icon">🗄️</div><h3>Supabase Backed</h3><p>Room state persisted to Postgres after every update. Sessions survive server restarts and redeploys.</p></div>
  <div class="card"><div class="card-icon">🌍</div><h3>Render Hosted</h3><p>Auto-deploys from GitHub on every commit. Scales with shared database state.</p></div>
  <div class="card"><div class="card-icon">🎵</div><h3>Queue Sharing</h3><p>Full queue sync with optional guest track-add permissions. Host maintains full control over playback.</p></div>
  <div class="card"><div class="card-icon">👥</div><h3>Host Controls</h3><p>Approval-required join, kick &amp; ban, and per-room settings. Built for collaborative listening.</p></div>
</div>
<div class="status-section">
  <div class="status-card">
    <div>
      <div class="status-label">API Status</div>
      <div class="status-value"><span class="status-dot"></span>All systems operational</div>
    </div>
    <div class="endpoints">
      <div class="endpoint"><span class="method post">POST</span>/v1/auth/token<span class="path-desc">Issue JWT</span></div>
      <div class="endpoint"><span class="method post">POST</span>/v1/together/sessions<span class="path-desc">Create room</span></div>
      <div class="endpoint"><span class="method post">POST</span>/v1/together/sessions/resolve<span class="path-desc">Join by code</span></div>
      <div class="endpoint"><span class="method ws">WSS</span>/v1/together/ws<span class="path-desc">Real-time sync</span></div>
      <div class="endpoint"><span class="method get">GET</span>/health<span class="path-desc">Health check</span></div>
    </div>
  </div>
</div>
<footer>
  <span class="footer-copy">© 2026 Koiverse. Sonora is open source.</span>
  <a href="https://github.com/koiverse/Sonora">GitHub</a>
  <a href="/health">Status</a>
</footer>
</body>
</html>`

// ────────────────────── Handlers ───────────────────────────────

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "Method not allowed")
		return
	}
	var req struct {
		Scopes []string `json:"scopes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	set := map[string]bool{}
	for _, sc := range req.Scopes {
		n := strings.ToLower(strings.TrimSpace(sc))
		if n == "together:rw" || n == "canvas:read" {
			set[n] = true
		}
	}
	if len(set) == 0 {
		set["together:rw"] = true
	}
	parts := make([]string, 0, len(set))
	for k := range set {
		parts = append(parts, k)
	}
	resp, err := issueToken(s.cfg, strings.Join(parts, " "))
	if err != nil {
		writeError(w, 500, "Failed to issue token")
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "Method not allowed")
		return
	}
	var req CreateSessionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "Invalid request")
		return
	}
	if strings.TrimSpace(req.HostDisplayName) == "" {
		req.HostDisplayName = "Host"
	}
	now := time.Now().UTC()
	rec := RoomRecord{
		SessionID: randomHex(16), Code: randomCode(6),
		HostKey: randomHex(20), GuestKey: randomHex(20), HostID: randomHex(12),
		Settings: req.Settings, CreatedAt: now, UpdatedAt: now,
		ExpiresAt: now.Add(s.cfg.RoomTTL),
	}
	if err := s.db.CreateRoom(r.Context(), rec); err != nil {
		log.Printf("createRoom: %v", err)
		writeError(w, 500, "Failed to create session")
		return
	}
	s.cache.set(newRoom(rec))
	writeJSON(w, 200, CreateSessionResp{
		SessionID: rec.SessionID, Code: rec.Code,
		HostKey: rec.HostKey, GuestKey: rec.GuestKey,
		WsURL: wsURL(r), Settings: rec.Settings,
	})
}

func (s *Server) handleResolveSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "Method not allowed")
		return
	}
	var req ResolveSessionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "Invalid request")
		return
	}
	rec, err := s.db.GetRoomByCode(r.Context(), req.Code)
	if err != nil {
		log.Printf("resolveSession: %v", err)
		writeError(w, 500, "Internal error")
		return
	}
	if rec == nil {
		writeError(w, 404, "Session not found")
		return
	}
	writeJSON(w, 200, ResolveSessionResp{
		SessionID: rec.SessionID, GuestKey: rec.GuestKey,
		WsURL: wsURL(r), Settings: rec.Settings,
	})
}

// ─────────────────────── WebSocket ─────────────────────────────

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Step 1 – read client_hello (10 s timeout)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})

	var hello ClientHello
	if err := json.Unmarshal(raw, &hello); err != nil || hello.Type != "client_hello" {
		s.sendErr(conn, "", "Expected client_hello", "PROTO_ERROR")
		return
	}

	// Step 2 – load room (cache → DB)
	ctx := r.Context()
	room, err := s.loadRoom(ctx, hello.SessionID)
	if err != nil {
		s.sendErr(conn, hello.SessionID, "Internal error", "SERVER_ERROR")
		return
	}
	if room == nil {
		s.sendErr(conn, hello.SessionID, "Session not found", "SESSION_NOT_FOUND")
		return
	}

	// Step 3 – authenticate peer
	peer, welcome, authErr := s.addPeer(room, conn, hello)
	if authErr != nil {
		s.sendErr(conn, hello.SessionID, authErr.Error(), "AUTH_FAILED")
		return
	}

	// Step 4 – send server_welcome
	if err := s.write(conn, welcome); err != nil {
		s.removePeer(room, peer)
		return
	}

	// Step 5 – send current room_state (if available)
	if st := room.getState(); st != nil {
		st.Participants = room.snapshotParticipants()
		_ = s.write(conn, RoomStateMsg{Type: "room_state", State: *st})
	}

	// Step 6 – notify others (unless pending approval)
	if !peer.Participant.IsPending {
		s.broadcastJoined(room, peer.Participant)
	}

	// Step 7 – message loop
	for {
		_, msgRaw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var env Inbound
		if err := json.Unmarshal(msgRaw, &env); err != nil {
			continue
		}
		env.Raw = msgRaw
		s.dispatch(ctx, room, peer, env)
	}

	// Step 8 – cleanup
	reason := "disconnected"
	s.removePeer(room, peer)
	s.broadcastLeft(room, peer.Participant.ID, &reason)

	room.mu.RLock()
	empty := len(room.participants) == 0
	room.mu.RUnlock()
	if empty {
		if st := room.getState(); st != nil {
			_ = s.db.SaveRoomState(ctx, room.Record.SessionID, *st, time.Now().UTC().Add(s.cfg.RoomTTL))
		}
		s.cache.del(room.Record.SessionID)
	}
}

func (s *Server) loadRoom(ctx context.Context, sessionID string) (*Room, error) {
	if r := s.cache.get(sessionID); r != nil {
		return r, nil
	}
	rec, err := s.db.GetRoomBySessionID(ctx, sessionID)
	if err != nil || rec == nil {
		return nil, err
	}
	r := newRoom(*rec)
	s.cache.set(r)
	return r, nil
}

func (s *Server) addPeer(room *Room, conn *websocket.Conn, hello ClientHello) (*Peer, ServerWelcome, error) {
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

	// Role MUST be uppercase – matches Kotlin enum ServerRole { HOST, GUEST }
	role, pid := "GUEST", randomHex(12)
	if isHost {
		role, pid = "HOST", room.Record.HostID
	}
	pending := isGuest && room.Record.Settings.RequireHostApprovalToJoin

	peer := &Peer{
		Participant: TogetherParticipant{
			ID: pid, Name: hello.DisplayName,
			IsHost: isHost, IsPending: pending, IsConnected: true,
		},
		Conn: conn, IsHost: isHost, Approved: !pending, ClientID: hello.ClientID,
	}
	room.participants[pid] = peer
	if isHost {
		room.hostPeer = peer
	}
	if pending {
		go func() {
			room.mu.RLock()
			h := room.hostPeer
			room.mu.RUnlock()
			if h != nil {
				_ = s.write(h.Conn, JoinRequest{
					Type: "join_request", SessionID: room.Record.SessionID,
					Participant: peer.Participant,
				})
			}
		}()
	}
	return peer, ServerWelcome{
		Type: "server_welcome", ProtocolVersion: hello.ProtocolVersion,
		SessionID: room.Record.SessionID, ParticipantID: pid,
		Role: role, IsPending: pending, Settings: room.Record.Settings,
	}, nil
}

func (s *Server) removePeer(room *Room, peer *Peer) {
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

// ─────────────────────── Dispatch ──────────────────────────────

func (s *Server) dispatch(ctx context.Context, room *Room, peer *Peer, env Inbound) {
	switch env.Type {

	// Host pushes full playback state → persist + fan-out to guests
	case "room_state":
		if !peer.IsHost {
			return
		}
		var msg HostRoomState
		if err := json.Unmarshal(env.Raw, &msg); err != nil {
			return
		}
		msg.State.Participants = room.snapshotParticipants()
		room.setState(msg.State)
		go func() {
			if err := s.db.SaveRoomState(ctx, room.Record.SessionID, msg.State, time.Now().UTC().Add(s.cfg.RoomTTL)); err != nil {
				log.Printf("SaveRoomState: %v", err)
			}
		}()
		s.broadcastApproved(room, RoomStateMsg{Type: "room_state", State: msg.State}, peer)

	// Guest requests playback control → forward to host
	case "control_request":
		if !peer.IsHost && !room.Record.Settings.AllowGuestsToControlPlayback {
			return
		}
		var msg ControlRequest
		if err := json.Unmarshal(env.Raw, &msg); err != nil {
			return
		}
		msg.ParticipantID = peer.Participant.ID
		if h := s.host(room); h != nil && h != peer {
			_ = s.write(h.Conn, msg)
		}

	// Guest adds track → forward to host
	case "add_track_request":
		if !peer.IsHost && !room.Record.Settings.AllowGuestsToAddTracks {
			return
		}
		var msg AddTrackRequest
		if err := json.Unmarshal(env.Raw, &msg); err != nil {
			return
		}
		msg.ParticipantID = peer.Participant.ID
		if h := s.host(room); h != nil && h != peer {
			_ = s.write(h.Conn, msg)
		}

	// Host approves/rejects pending guest
	case "join_decision":
		if !peer.IsHost {
			return
		}
		var msg JoinDecision
		if err := json.Unmarshal(env.Raw, &msg); err != nil {
			return
		}
		room.mu.Lock()
		target := room.participants[msg.ParticipantID]
		if target != nil {
			target.Approved = msg.Approved
			target.Participant.IsPending = false
		}
		room.mu.Unlock()
		if target == nil {
			return
		}
		if msg.Approved {
			s.broadcastJoined(room, target.Participant)
			if st := room.getState(); st != nil {
				st.Participants = room.snapshotParticipants()
				_ = s.write(target.Conn, RoomStateMsg{Type: "room_state", State: *st})
			}
		} else {
			r := "rejected"
			_ = s.write(target.Conn, ParticipantLeft{
				Type: "participant_left", SessionID: room.Record.SessionID,
				ParticipantID: target.Participant.ID, Reason: &r,
			})
		}

	case "kick":
		if !peer.IsHost {
			return
		}
		var msg KickParticipant
		if err := json.Unmarshal(env.Raw, &msg); err != nil {
			return
		}
		if t := s.peer(room, msg.ParticipantID); t != nil && !t.IsHost {
			r := "kicked"
			_ = s.write(t.Conn, ParticipantLeft{
				Type: "participant_left", SessionID: room.Record.SessionID,
				ParticipantID: t.Participant.ID, Reason: &r,
			})
		}

	case "ban":
		if !peer.IsHost {
			return
		}
		var msg BanParticipant
		if err := json.Unmarshal(env.Raw, &msg); err != nil {
			return
		}
		t := s.peer(room, msg.ParticipantID)
		if t == nil || t.IsHost {
			return
		}
		room.mu.Lock()
		room.banned[t.ClientID] = true
		room.mu.Unlock()
		r := "banned"
		_ = s.write(t.Conn, ParticipantLeft{
			Type: "participant_left", SessionID: room.Record.SessionID,
			ParticipantID: t.Participant.ID, Reason: &r,
		})

	case "heartbeat_ping":
		var msg HeartbeatPing
		if err := json.Unmarshal(env.Raw, &msg); err != nil {
			return
		}
		_ = s.write(peer.Conn, HeartbeatPong{
			Type: "heartbeat_pong", SessionID: msg.SessionID,
			PingID: msg.PingID, ClientElapsedRealtime: msg.ClientElapsedRealtime,
			ServerElapsedRealtime: time.Now().UnixMilli(),
		})

	case "client_leave":
		// WS close will trigger cleanup — nothing extra needed
	}
}

// ─────────────────────── Broadcast helpers ─────────────────────

func (s *Server) host(room *Room) *Peer {
	room.mu.RLock()
	defer room.mu.RUnlock()
	return room.hostPeer
}

func (s *Server) peer(room *Room, id string) *Peer {
	room.mu.RLock()
	defer room.mu.RUnlock()
	return room.participants[id]
}

func (s *Server) broadcastJoined(room *Room, p TogetherParticipant) {
	s.broadcastApproved(room, ParticipantJoined{
		Type: "participant_joined", SessionID: room.Record.SessionID, Participant: p,
	}, nil)
}

func (s *Server) broadcastLeft(room *Room, pid string, reason *string) {
	s.broadcastApproved(room, ParticipantLeft{
		Type: "participant_left", SessionID: room.Record.SessionID,
		ParticipantID: pid, Reason: reason,
	}, nil)
}

func (s *Server) broadcastApproved(room *Room, payload any, exclude *Peer) {
	room.mu.RLock()
	peers := make([]*Peer, 0, len(room.participants))
	for _, p := range room.participants {
		if p != exclude && p.Approved && p.Conn != nil {
			peers = append(peers, p)
		}
	}
	room.mu.RUnlock()
	for _, p := range peers {
		_ = s.write(p.Conn, payload)
	}
}

func (s *Server) write(conn *websocket.Conn, payload any) error {
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

func (s *Server) sendErr(conn *websocket.Conn, sessionID, message, code string) {
	var sid *string
	if sessionID != "" {
		sid = &sessionID
	}
	c := code
	_ = s.write(conn, ServerError{Type: "server_error", SessionID: sid, Message: message, Code: &c})
}

// Canvas proxy ──────────────────────────────────────────────────

func (s *Server) handleCanvas(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CanvasUpstreamBase == "" {
		writeError(w, 503, "Canvas upstream not configured")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method,
		s.cfg.CanvasUpstreamBase+r.URL.RequestURI(), r.Body)
	if err != nil {
		writeError(w, 500, "Proxy error")
		return
	}
	req.Header = r.Header.Clone()
	if s.cfg.CanvasUpstreamToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.CanvasUpstreamToken)
	}
	resp, err := s.client.Do(req)
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

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return time.Now().Format("150405.000000")
	}
	return hex.EncodeToString(b)
}

func randomCode(n int) string {
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	out := make([]byte, n)
	for i := range out {
		out[i] = alpha[int(raw[i])%len(alpha)]
	}
	return string(out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string, codes ...string) {
	m := map[string]any{"ok": false, "error": msg}
	if len(codes) > 0 {
		m["code"] = codes[0]
	}
	writeJSON(w, status, m)
}

func wsURL(r *http.Request) string {
	scheme := "ws"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "wss"
	}
	host := r.Host
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		host = strings.TrimSpace(xfh)
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
	log.Println("✓ database connected")

	if err := db.Migrate(context.Background()); err != nil {
		log.Fatalf("migration failed: %v", err)
	}
	log.Println("✓ schema ready")

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			if n, err := db.DeleteExpiredRooms(context.Background()); err != nil {
				log.Printf("janitor: %v", err)
			} else if n > 0 {
				log.Printf("janitor: removed %d expired rooms", n)
			}
		}
	}()

	srv := newServer(cfg, db)
	log.Printf("✓ Sonora Together listening on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, srv.routes()); err != nil {
		log.Fatalf("server: %v", err)
	}
}
