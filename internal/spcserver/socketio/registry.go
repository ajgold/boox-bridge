package socketio

import (
	"sync"

	"github.com/gorilla/websocket"
)

// wsWriter is the subset of *websocket.Conn the registry needs, so connections
// can be faked in tests. *websocket.Conn satisfies it.
type wsWriter interface {
	WriteMessage(messageType int, data []byte) error
}

// Conn is a live device connection: a websocket plus its identity. Writes are
// serialized by mu (gorilla forbids concurrent writes to one conn), mirroring
// internal/sync/notifier.go's writeMu.
type Conn struct {
	UserID string
	ws     wsWriter
	mu     sync.Mutex
}

// NewConn wraps a writer (a *websocket.Conn in production) with an identity.
func NewConn(userID string, ws wsWriter) *Conn {
	return &Conn{UserID: userID, ws: ws}
}

// write sends one text frame, serialized against concurrent writers.
func (c *Conn) write(frame []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, frame)
}

// Registry maps userId → its set of live connections. Safe for concurrent use.
type Registry struct {
	mu    sync.Mutex
	conns map[string]map[*Conn]struct{}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{conns: make(map[string]map[*Conn]struct{})}
}

// Add registers a connection under its UserID.
func (r *Registry) Add(c *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.conns[c.UserID]
	if set == nil {
		set = make(map[*Conn]struct{})
		r.conns[c.UserID] = set
	}
	set[c] = struct{}{}
}

// Remove deregisters a connection, dropping the user's entry when empty.
func (r *Registry) Remove(c *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.conns[c.UserID]
	if set == nil {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(r.conns, c.UserID)
	}
}

// Emit frames event+payload as a Socket.IO event and writes it to each of
// userID's live connections, returning the number successfully delivered. It
// no-ops (returns 0) when the user has no connection — UB has no offline queue
// (see docs/future-work/spc-no-analogue-features.md); a missed nudge is caught
// by the device's next periodic sync.
func (r *Registry) Emit(userID, event string, payload any) (delivered int) {
	r.mu.Lock()
	targets := make([]*Conn, 0, len(r.conns[userID]))
	for c := range r.conns[userID] {
		targets = append(targets, c)
	}
	r.mu.Unlock()

	frame := EncodeEvent(event, payload)
	for _, c := range targets {
		if err := c.write(frame); err == nil {
			delivered++
		}
	}
	return delivered
}
