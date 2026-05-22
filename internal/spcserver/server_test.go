package spcserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestServer builds a Server with no DB (the routes exercised here don't
// touch it).
func newTestServer() *Server {
	return New(Config{Mode: "server", JWTSecret: "s"})
}

// TestSocketIOMountedSameListener verifies /socket.io/ is served on the same
// mux as /api/* — an unauthenticated request reaches the socket handler's auth
// gate (401), not a 404. Verifies: spc-phase-1.AC3.1 (single listener)
func TestSocketIOMountedSameListener(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/socket.io/?EIO=3&transport=websocket", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("/socket.io/ not mounted (404)")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 from socket auth gate, got %d", rec.Code)
	}

	// Sanity: /api/* still served on the same mux.
	apiReq := httptest.NewRequest(http.MethodPost, "/api/equipment/bind/status", nil)
	apiRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Errorf("bind/status on same mux: got %d", apiRec.Code)
	}
}

// TestSocketRegistryExposed verifies the registry is available for 1d pushes.
func TestSocketRegistryExposed(t *testing.T) {
	if newTestServer().SocketRegistry() == nil {
		t.Errorf("SocketRegistry() returned nil")
	}
}
