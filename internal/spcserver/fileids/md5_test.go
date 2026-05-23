package fileids

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/notedb"
)

func md5Hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func newRegForMD5(t *testing.T) *Registry {
	t.Helper()
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return New(db, t.TempDir())
}

// TestMD5ForComputesGolden verifies the first call returns the correct lowercase MD5 hex.
func TestMD5ForComputesGolden(t *testing.T) {
	ctx := context.Background()
	reg := newRegForMD5(t)
	p := filepath.Join(t.TempDir(), "f.bin")
	content := []byte("hello supernote")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := reg.MD5For(ctx, p)
	if err != nil {
		t.Fatalf("MD5For: %v", err)
	}
	if want := md5Hex(content); got != want {
		t.Errorf("MD5For = %q; want %q", got, want)
	}
}

// TestMD5ForCachesUntilStatChanges verifies a second call serves the cached digest
// (it does not re-read the bytes) and that bumping mtime invalidates the cache.
func TestMD5ForCachesUntilStatChanges(t *testing.T) {
	ctx := context.Background()
	reg := newRegForMD5(t)
	p := filepath.Join(t.TempDir(), "f.bin")
	orig := []byte("AAAA")
	if err := os.WriteFile(p, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	origMtime := fi.ModTime()

	first, err := reg.MD5For(ctx, p)
	if err != nil {
		t.Fatalf("MD5For first: %v", err)
	}

	// Rewrite same-length content, then restore the original mtime. Because
	// size+mtime are unchanged, MD5For must return the CACHED digest (proving it
	// did not re-read the file), even though the bytes on disk differ.
	if err := os.WriteFile(p, []byte("BBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, origMtime, origMtime); err != nil {
		t.Fatal(err)
	}
	cached, err := reg.MD5For(ctx, p)
	if err != nil {
		t.Fatalf("MD5For cached: %v", err)
	}
	if cached != first {
		t.Errorf("expected cached digest %q (no re-read), got %q", first, cached)
	}

	// Now bump mtime → cache invalidated → recompute against the new bytes.
	newMtime := origMtime.Add(2 * time.Second)
	if err := os.Chtimes(p, newMtime, newMtime); err != nil {
		t.Fatal(err)
	}
	recomputed, err := reg.MD5For(ctx, p)
	if err != nil {
		t.Fatalf("MD5For recompute: %v", err)
	}
	if want := md5Hex([]byte("BBBB")); recomputed != want {
		t.Errorf("after mtime bump MD5For = %q; want %q (recomputed)", recomputed, want)
	}
}
