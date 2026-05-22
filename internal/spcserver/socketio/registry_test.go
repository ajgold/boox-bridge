package socketio

import (
	"sync"
	"testing"
)

// fakeWS captures frames written to it.
type fakeWS struct {
	mu     sync.Mutex
	frames [][]byte
}

func (f *fakeWS) WriteMessage(_ int, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, append([]byte(nil), data...))
	return nil
}

func (f *fakeWS) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.frames) == 0 {
		return ""
	}
	return string(f.frames[len(f.frames)-1])
}

// TestEmitDelivers verifies Add then Emit writes the framed event to the user's
// connection. Verifies: spc-phase-1.AC3.6
func TestEmitDelivers(t *testing.T) {
	reg := NewRegistry()
	ws := &fakeWS{}
	reg.Add(NewConn("user1", ws))

	n := reg.Emit("user1", "ServerMessage", map[string]string{"op": "STARTSYNC"})
	if n != 1 {
		t.Errorf("delivered: got %d, want 1", n)
	}
	if got := ws.last(); got != `42["ServerMessage",{"op":"STARTSYNC"}]` {
		t.Errorf("frame: got %q", got)
	}
}

// TestEmitUnknownUserNoOp verifies Emit to an absent userId delivers nothing.
// Verifies: spc-phase-1.AC3.6
func TestEmitUnknownUserNoOp(t *testing.T) {
	reg := NewRegistry()
	if n := reg.Emit("nobody", "ServerMessage", nil); n != 0 {
		t.Errorf("delivered to absent user: got %d, want 0", n)
	}
}

// TestRemoveStopsDelivery verifies a removed conn no longer receives.
func TestRemoveStopsDelivery(t *testing.T) {
	reg := NewRegistry()
	ws := &fakeWS{}
	c := NewConn("user1", ws)
	reg.Add(c)
	reg.Remove(c)

	if n := reg.Emit("user1", "ServerMessage", nil); n != 0 {
		t.Errorf("delivered after Remove: got %d, want 0", n)
	}
}

// TestEmitMultipleConns verifies all of a user's live conns receive (e.g. two
// devices on one account).
func TestEmitMultipleConns(t *testing.T) {
	reg := NewRegistry()
	a, b := &fakeWS{}, &fakeWS{}
	reg.Add(NewConn("user1", a))
	reg.Add(NewConn("user1", b))

	if n := reg.Emit("user1", "ratta_ping", "Received"); n != 2 {
		t.Errorf("delivered: got %d, want 2", n)
	}
}
