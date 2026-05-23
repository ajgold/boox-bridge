// Package capacity computes the storage-usage number UB reports to the device.
// It is a leaf package (no internal imports) so both the spcserver wiring and
// the handlers can use it without an import cycle. The "used" figure is a
// du-style recursive sum of file sizes under the SPC file root, cached briefly
// because the device polls it during sync and a full walk is not free.
package capacity

import (
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

const ttl = 60 * time.Second

// Meter reports recursive byte usage under root against a fixed quota.
type Meter struct {
	root  string
	quota int64

	mu     sync.Mutex
	cached int64
	at     time.Time
	now    func() time.Time // injectable for tests
}

// New builds a Meter for the given root and total-capacity quota. An empty root
// reports zero usage (file listing disabled).
func New(root string, quota int64) *Meter {
	return &Meter{root: root, quota: quota, now: time.Now}
}

// Quota returns the configured total capacity.
func (m *Meter) Quota() int64 { return m.quota }

// Usage returns the recursive byte sum of regular files under root, recomputing
// at most once per TTL. Walk errors (a vanished or unreadable file) are skipped
// so a transient FS hiccup never fails the meter.
func (m *Meter) Usage() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.at.IsZero() && m.now().Sub(m.at) < ttl {
		return m.cached
	}

	var sum int64
	if m.root != "" {
		_ = filepath.WalkDir(m.root, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries, keep walking
			}
			if d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					sum += info.Size()
				}
			}
			return nil
		})
	}
	m.cached = sum
	m.at = m.now()
	return sum
}
