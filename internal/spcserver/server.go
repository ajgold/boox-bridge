// Package spcserver is the device-facing reimplementation of the Supernote
// Private Cloud (SPC) protocol, letting an unmodified Supernote device talk to
// UltraBridge as if it were the real SPC server. It owns the HTTP listener and
// (from 1c) the Engine.IO server, wiring the handlers/auth/socketio subpackages
// onto a single mux. See internal/spcserver/CLAUDE.md and docs/spc-protocol.md.
package spcserver

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/sysop/ultrabridge/internal/spcserver/auth"
	"github.com/sysop/ultrabridge/internal/spcserver/handlers"
	"github.com/sysop/ultrabridge/internal/spcserver/login"
)

// Config holds the SPC server's runtime configuration, populated from appconfig
// in cmd/ultrabridge/main.go.
type Config struct {
	Mode       string // "client" (no listener) | "server"
	ListenAddr string
	TLSCert    string
	TLSKey     string
	// DB is the shared notedb handle. Handlers persist/read SPC runtime state
	// (e.g. the harvested spc_user_id) through it via notedb.GetSetting/SetSetting.
	DB *sql.DB
	// JWTSecret signs/verifies device tokens (Constant.SECRET by default).
	JWTSecret string
	// DeviceAccount/DevicePassword are the expected terminal-login credentials;
	// DeviceAccount "" accepts any account. DevicePassword is the raw password.
	DeviceAccount  string
	DevicePassword string
	Logger         *slog.Logger
}

// Server is the SPC HTTP (and, from 1c, Engine.IO) server. It is constructed
// only when Mode == "server"; in "client" mode main.go never calls New.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// New builds the server, registering all routes on its mux.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.registerRoutes()
	return s
}

// Handler exposes the mux for in-process tests (httptest) without binding a
// socket.
func (s *Server) Handler() http.Handler { return s.mux }

// registerRoutes wires the device-facing endpoints. Go 1.22 method+path
// patterns match the routing style already used in cmd/ultrabridge/main.go.
// Login/challenge/boot routes are unauthenticated (the device has no token yet);
// business endpoints are wrapped with auth.Middleware.
func (s *Server) registerRoutes() {
	store := settingStore{db: s.cfg.DB}
	lh := &handlers.LoginHandler{
		DeviceAccount:  s.cfg.DeviceAccount,
		DevicePassword: s.cfg.DevicePassword,
		JWTSecret:      s.cfg.JWTSecret,
		Codes:          login.NewStore(),
		Store:          store,
	}

	// Equipment status (1a) — unauthenticated stub the device polls.
	s.mux.HandleFunc("POST /api/equipment/bind/status", handlers.BindStatus)

	// Login + challenge + boot handshake — all unauthenticated.
	s.mux.HandleFunc("POST /api/official/user/query/random/code", lh.RandomCode)
	s.mux.HandleFunc("POST /api/official/user/check/exists/server", lh.CheckExistsServer)
	s.mux.HandleFunc("POST /api/official/user/account/login/equipment", lh.Login)
	s.mux.HandleFunc("POST /api/official/user/account/login/new", lh.Login)
	s.mux.HandleFunc("POST /api/user/query/token", lh.QueryToken)
	s.mux.HandleFunc("POST /api/user/logout", lh.Logout)
	s.mux.HandleFunc("POST /api/terminal/user/bindEquipment", lh.BindEquipment)
	s.mux.HandleFunc("POST /api/terminal/equipment/unlink", lh.Unlink)
	s.mux.HandleFunc("GET /api/file/query/server", lh.FileQueryServer)

	// Protected probe — requires a valid x-access-token (1b).
	s.mux.Handle("POST /api/user/query",
		auth.Middleware(s.cfg.JWTSecret, store, http.HandlerFunc(handlers.UserQuery)))
}

// Run binds the listener and serves until error. TLS is used when both cert and
// key are set; otherwise plain HTTP (TLS is typically terminated upstream by
// the reverse proxy in this deployment).
func (s *Server) Run() error {
	if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
		return http.ListenAndServeTLS(s.cfg.ListenAddr, s.cfg.TLSCert, s.cfg.TLSKey, s.mux)
	}
	return http.ListenAndServe(s.cfg.ListenAddr, s.mux)
}
