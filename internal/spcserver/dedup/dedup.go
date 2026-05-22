// Package dedup implements SPC's ResubmitCheck: an in-memory short-window guard
// against duplicate mutating requests (the device occasionally re-POSTs an
// identical create). Mirrors com/ratta/.../ResubmitCheck.java with its default
// 1-second interval. Single-process, single-user — no Redis (see
// docs/future-work/spc-no-analogue-features.md).
package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// defaultTTL is ResubmitCheck's default interval.
const defaultTTL = time.Second

// Checker records recently-seen (userID, endpoint, body) keys and reports
// repeats within the TTL.
type Checker struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
	now  func() time.Time
}

// NewChecker returns a Checker with the default 1s window.
func NewChecker() *Checker {
	return &Checker{seen: make(map[string]time.Time), ttl: defaultTTL, now: time.Now}
}

// Seen returns true if an identical request was recorded within the TTL,
// otherwise records it and returns false. Expiry is lazy with an opportunistic
// sweep on each call — no background goroutine.
func (c *Checker) Seen(userID, endpoint string, body []byte) bool {
	key := userID + "|" + endpoint + "|" + sha256hex(body)

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, ts := range c.seen {
		if now.Sub(ts) > c.ttl {
			delete(c.seen, k)
		}
	}
	if ts, ok := c.seen[key]; ok && now.Sub(ts) <= c.ttl {
		return true
	}
	c.seen[key] = now
	return false
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
