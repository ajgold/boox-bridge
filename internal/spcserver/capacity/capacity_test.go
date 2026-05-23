package capacity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestUsageSumsRecursively verifies the du-style recursive byte sum.
func TestUsageSumsRecursively(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.bin"), 100)
	writeFile(t, filepath.Join(root, "Note", "b.note"), 250)
	writeFile(t, filepath.Join(root, "Note", "Sub", "c.note"), 50)

	m := New(root, 1<<40)
	if got := m.Usage(); got != 400 {
		t.Errorf("Usage = %d; want 400", got)
	}
	if m.Quota() != 1<<40 {
		t.Errorf("Quota = %d; want 1 TiB", m.Quota())
	}
}

// TestUsageCachedWithinWindow verifies a second call inside the TTL returns the
// cached value (a file added after the first call is not reflected), and that
// advancing the clock past the TTL forces a re-walk.
func TestUsageCachedWithinWindow(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.bin"), 100)

	now := time.Unix(1000, 0)
	m := New(root, 1<<40)
	m.now = func() time.Time { return now }

	if got := m.Usage(); got != 100 {
		t.Fatalf("first Usage = %d; want 100", got)
	}
	// Add 200 bytes, but stay within the 60s window → still cached at 100.
	writeFile(t, filepath.Join(root, "b.bin"), 200)
	now = now.Add(30 * time.Second)
	if got := m.Usage(); got != 100 {
		t.Errorf("cached Usage = %d; want 100 (within TTL)", got)
	}
	// Advance past the TTL → re-walk picks up the new file.
	now = now.Add(61 * time.Second)
	if got := m.Usage(); got != 300 {
		t.Errorf("post-TTL Usage = %d; want 300 (re-walked)", got)
	}
}

// TestUsageEmptyRoot verifies an empty/unset root yields 0 without error.
func TestUsageEmptyRoot(t *testing.T) {
	m := New("", 1<<40)
	if got := m.Usage(); got != 0 {
		t.Errorf("Usage(empty root) = %d; want 0", got)
	}
}
