package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// dedup persists a SHA-256 skip-list so that Boox's habit of re-uploading
// the full ZIP on every edit doesn't generate duplicate Affine docs.
type dedup struct {
	mu   sync.Mutex
	path string
	seen map[string]string // hash -> ISO-8601 first-seen time
}

func openDedup(stateDir string) (*dedup, error) {
	d := &dedup{
		path: filepath.Join(stateDir, "seen.json"),
		seen: make(map[string]string),
	}
	b, err := os.ReadFile(d.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return d, nil
		}
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &d.seen); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// size returns the number of hashes currently in the skip-list.
func (d *dedup) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// has reports whether the hash is already in the skip-list.
func (d *dedup) has(hash string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.seen[hash]
	return ok
}

// mark records the hash as seen and flushes to disk.
func (d *dedup) mark(hash string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[hash]; ok {
		return nil
	}
	d.seen[hash] = time.Now().UTC().Format(time.RFC3339)
	return d.flushLocked()
}

func (d *dedup) flushLocked() error {
	b, err := json.MarshalIndent(d.seen, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, d.path)
}
