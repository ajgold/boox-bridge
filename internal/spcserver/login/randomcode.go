package login

import (
	"crypto/rand"
	"math/big"
	"sync"
	"time"
)

// codeTTL is how long an issued randomCode stays valid. SPC uses ~5 min.
const codeTTL = 5 * time.Minute

type codeEntry struct {
	code     string
	issuedAt time.Time
}

// Store is an in-memory, one-time randomCode store keyed by account. It is the
// UB analogue of SPC's Redis-backed code cache (single process, single user, so
// no Redis needed). Expiry is lazy (checked on Consume) plus an opportunistic
// sweep on Issue — no background goroutine, which keeps it leak-free and
// trivially testable via the injectable now func.
type Store struct {
	mu    sync.Mutex
	codes map[string]codeEntry
	ttl   time.Duration
	now   func() time.Time
}

// NewStore returns an empty randomCode store with the default 5-minute TTL.
func NewStore() *Store {
	return &Store{
		codes: make(map[string]codeEntry),
		ttl:   codeTTL,
		now:   time.Now,
	}
}

// Issue generates and stores a fresh one-time code for account, replacing any
// previous unconsumed code. It also sweeps expired entries.
func (s *Store) Issue(account string) string {
	code := randomCode()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.codes[account] = codeEntry{code: code, issuedAt: s.now()}
	return code
}

// Consume returns and deletes account's code if one exists and is unexpired.
// The second return is false when there is no live code (one-time semantics).
func (s *Store) Consume(account string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.codes[account]
	if !ok {
		return "", false
	}
	delete(s.codes, account)
	if s.now().Sub(e.issuedAt) > s.ttl {
		return "", false
	}
	return e.code, true
}

// sweepLocked drops expired entries; caller holds s.mu.
func (s *Store) sweepLocked() {
	now := s.now()
	for acct, e := range s.codes {
		if now.Sub(e.issuedAt) > s.ttl {
			delete(s.codes, acct)
		}
	}
}

// randomCode returns a short numeric one-time code. The exact format is
// cosmetic — the device echoes it back inside the password hash.
func randomCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		// rand.Reader failure is not expected; fall back to a fixed-width zero.
		return "000000"
	}
	return pad6(n.Int64())
}

func pad6(n int64) string {
	const digits = "0123456789"
	b := []byte("000000")
	for i := 5; i >= 0; i-- {
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b)
}
