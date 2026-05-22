package login

import (
	"context"
	"regexp"
	"testing"
	"time"
)

// Golden values computed independently with md5sum/sha256sum, locking the SPC
// password recipe (docs/spc-protocol.md §2.1) end-to-end:
//
//	md5Hex("ehh1701jqb")           = 632ae9a09cc341343c82421579d3afbc
//	sha256Hex(md5Hex+"Y")          = fcfa0d5a...ea292ef3
const (
	goldenRaw    = "ehh1701jqb"
	goldenCode   = "Y"
	goldenMD5    = "632ae9a09cc341343c82421579d3afbc"
	goldenWebPwd = "fcfa0d5a9c27800b6bfd24ce28bd18c52d019fd6d466f5d13df32206ea292ef3"
)

// TestHashGoldens locks md5Hex and sha256Hex against precomputed values.
// Verifies: spc-phase-1.AC2.6
func TestHashGoldens(t *testing.T) {
	if got := Md5Hex(goldenRaw); got != goldenMD5 {
		t.Errorf("Md5Hex(%q) = %q, want %q", goldenRaw, got, goldenMD5)
	}
	if got := Sha256Hex(goldenMD5 + goldenCode); got != goldenWebPwd {
		t.Errorf("Sha256Hex(md5+code) = %q, want %q", got, goldenWebPwd)
	}
	if got := ServicePassword(goldenRaw); got != goldenMD5 {
		t.Errorf("ServicePassword(%q) = %q, want %q", goldenRaw, got, goldenMD5)
	}
}

// TestCheckWebPassword verifies the full recipe accepts the matching
// webPassword (incl. trailing whitespace) and rejects a wrong one.
// Verifies: spc-phase-1.AC2.6
func TestCheckWebPassword(t *testing.T) {
	if !CheckWebPassword(goldenRaw, goldenCode, goldenWebPwd) {
		t.Errorf("expected matching webPassword to be accepted")
	}
	if !CheckWebPassword(goldenRaw, goldenCode, "  "+goldenWebPwd+"  ") {
		t.Errorf("expected accepted after TrimSpace")
	}
	if CheckWebPassword(goldenRaw, goldenCode, "deadbeef") {
		t.Errorf("expected wrong webPassword to be rejected")
	}
	if CheckWebPassword(goldenRaw, "wrong-code", goldenWebPwd) {
		t.Errorf("expected wrong code to be rejected")
	}
}

// TestRandomCodeOneTime verifies Issue→Consume returns the code exactly once.
func TestRandomCodeOneTime(t *testing.T) {
	s := NewStore()
	code := s.Issue("alice@example.com")
	if code == "" {
		t.Fatalf("Issue returned empty code")
	}

	got, ok := s.Consume("alice@example.com")
	if !ok || got != code {
		t.Errorf("first Consume: got (%q,%v), want (%q,true)", got, ok, code)
	}
	if _, ok := s.Consume("alice@example.com"); ok {
		t.Errorf("second Consume should fail (one-time)")
	}
}

// TestRandomCodeExpiry verifies a code past its TTL is not consumable.
func TestRandomCodeExpiry(t *testing.T) {
	base := time.Now()
	s := NewStore()
	s.now = func() time.Time { return base }

	s.Issue("bob@example.com")
	s.now = func() time.Time { return base.Add(6 * time.Minute) } // TTL is 5 min

	if _, ok := s.Consume("bob@example.com"); ok {
		t.Errorf("expired code should not be consumable")
	}
}

// fakeStore is an in-memory SettingStore for ResolveUserID tests.
type fakeStore struct{ m map[string]string }

func (f *fakeStore) Get(_ context.Context, k string) (string, error) { return f.m[k], nil }
func (f *fakeStore) Set(_ context.Context, k, v string) error        { f.m[k] = v; return nil }

// TestResolveUserID verifies first call generates+persists a stable 19-digit
// numeric id and the second call returns the same value.
func TestResolveUserID(t *testing.T) {
	store := &fakeStore{m: map[string]string{}}
	ctx := context.Background()

	id1, err := ResolveUserID(ctx, store)
	if err != nil {
		t.Fatalf("ResolveUserID: %v", err)
	}
	if !regexp.MustCompile(`^[1-9][0-9]{18}$`).MatchString(id1) {
		t.Errorf("expected 19-digit numeric id, got %q", id1)
	}

	id2, err := ResolveUserID(ctx, store)
	if err != nil {
		t.Fatalf("ResolveUserID 2nd: %v", err)
	}
	if id2 != id1 {
		t.Errorf("ResolveUserID not stable: %q != %q", id2, id1)
	}
}
