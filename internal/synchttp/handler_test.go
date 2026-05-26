package synchttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/syncsvc"
)

type stubSyncer struct {
	resp   syncsvc.Response
	err    error
	gotReq syncsvc.Request
}

func (s *stubSyncer) Sync(_ context.Context, req syncsvc.Request) (syncsvc.Response, error) {
	s.gotReq = req
	return s.resp, s.err
}

func do(t *testing.T, h http.Handler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/sync/v1", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHandler_Success(t *testing.T) {
	stub := &stubSyncer{resp: syncsvc.Response{ProtocolVersion: 1, AcceptedThrough: 7, HasMore: true}}
	h := New(stub, DefaultMaxBytes, nil)

	body := `{"protocol_version":1,"schema_hash":"abc","site_id":"0000000000000000000000000A","cursor":3,"ops":[]}`
	w := do(t, h, http.MethodPost, body)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if stub.gotReq.SiteID != "0000000000000000000000000A" || stub.gotReq.Cursor != 3 {
		t.Errorf("request not parsed: %+v", stub.gotReq)
	}
	var resp syncsvc.Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.AcceptedThrough != 7 || !resp.HasMore {
		t.Errorf("resp = %+v, want accepted_through 7 / has_more true", resp)
	}
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	h := New(&stubSyncer{}, DefaultMaxBytes, nil)
	if w := do(t, h, http.MethodGet, ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", w.Code)
	}
}

func TestHandler_MalformedJSON(t *testing.T) {
	h := New(&stubSyncer{}, DefaultMaxBytes, nil)
	if w := do(t, h, http.MethodPost, "{not json"); w.Code != http.StatusBadRequest {
		t.Errorf("malformed status = %d, want 400", w.Code)
	}
}

func TestHandler_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"bad request", fmt.Errorf("%w: x", syncsvc.ErrBadRequest), http.StatusBadRequest},
		{"schema mismatch", fmt.Errorf("%w", syncsvc.ErrSchemaMismatch), http.StatusConflict},
		{"unsupported version", fmt.Errorf("%w", syncsvc.ErrUnsupportedVersion), http.StatusConflict},
		{"generic", fmt.Errorf("db exploded"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := New(&stubSyncer{err: c.err}, DefaultMaxBytes, nil)
			body := `{"protocol_version":1,"site_id":"0000000000000000000000000A"}`
			if w := do(t, h, http.MethodPost, body); w.Code != c.want {
				t.Errorf("status = %d, want %d", w.Code, c.want)
			}
		})
	}
}

func TestHandler_BodyTooLarge(t *testing.T) {
	h := New(&stubSyncer{}, 16, nil) // 16-byte cap
	big := `{"protocol_version":1,"site_id":"0000000000000000000000000A","ops":[]}`
	if w := do(t, h, http.MethodPost, big); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized status = %d, want 413; body=%s", w.Code, w.Body.String())
	}
}
