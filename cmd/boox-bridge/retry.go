package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rateLimitErr signals an HWR-side rate limit (HTTP 429 from Anthropic or the
// gateway) and carries the retry-after hint the upstream sent. Caught by the
// pipeline and turned into a deferred retry via the spool.
type rateLimitErr struct {
	RetryAfter time.Duration
	Message    string
}

func (e *rateLimitErr) Error() string {
	return fmt.Sprintf("rate limited (retry after %s): %s", e.RetryAfter, e.Message)
}

// parseRetryAfter accepts both the seconds-integer form and the HTTP-date
// form per RFC 7231. Returns 0 when the value is missing or unparseable —
// the caller substitutes a configured default.
func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if sec, err := strconv.Atoi(raw); err == nil && sec >= 0 {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// retryRecord is the JSON sidecar that lives alongside a queued .note.
// Persists across daemon restarts.
type retryRecord struct {
	BaseName      string    `json:"base_name"`
	AttemptCount  int       `json:"attempt_count"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastError     string    `json:"last_error"`
	FirstQueuedAt time.Time `json:"first_queued_at"`
}

// spool defers files that hit a transient failure (rate limit) and moves
// them back into the inbox when their retry window opens. Distinct from the
// DLQ (which is for permanent failures).
type spool struct {
	cfg *config

	mu   sync.Mutex
	tick chan struct{} // wakes the loop early when a short-delay retry is scheduled
}

func newSpool(cfg *config) *spool {
	return &spool{
		cfg:  cfg,
		tick: make(chan struct{}, 1),
	}
}

// errRetryExhausted means we've blown past MaxRetries and the file must
// land in the DLQ. The caller is expected to move the file.
var errRetryExhausted = errors.New("retry budget exhausted")

// schedule moves srcPath into retry/, writes (or updates) the sidecar,
// and returns the new attempt count. Returns errRetryExhausted when the
// caller should DLQ instead.
func (s *spool) schedule(srcPath string, delay time.Duration, lastErr string) (int, error) {
	if delay <= 0 {
		delay = time.Duration(s.cfg.DefaultRetryAfterSeconds) * time.Second
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.cfg.RetryDir(), 0o750); err != nil {
		return 0, err
	}
	base := filepath.Base(srcPath)
	sidecarPath := filepath.Join(s.cfg.RetryDir(), base+".retry.json")

	rec := retryRecord{BaseName: base, FirstQueuedAt: time.Now().UTC()}
	if data, err := os.ReadFile(sidecarPath); err == nil {
		_ = json.Unmarshal(data, &rec)
	}
	rec.AttemptCount++
	rec.NextAttemptAt = time.Now().Add(delay).UTC()
	rec.LastError = lastErr

	if rec.AttemptCount > s.cfg.MaxRetries {
		return rec.AttemptCount, errRetryExhausted
	}

	dst := filepath.Join(s.cfg.RetryDir(), base)
	// If the file is already in retry/ (e.g., we're updating after a failed
	// re-attempt), the rename targets itself — that's a no-op on POSIX.
	if srcPath != dst {
		if err := os.Rename(srcPath, dst); err != nil {
			return rec.AttemptCount, fmt.Errorf("move to retry: %w", err)
		}
	}
	sidecar, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return rec.AttemptCount, err
	}
	if err := os.WriteFile(sidecarPath, sidecar, 0o640); err != nil {
		return rec.AttemptCount, err
	}

	// Wake the run loop early if the delay is short enough that the next
	// scheduled wake would miss it.
	select {
	case s.tick <- struct{}{}:
	default:
	}
	return rec.AttemptCount, nil
}

// run loops until ctx is cancelled, requeueing any files whose retry
// window has opened.
func (s *spool) run(ctx context.Context) {
	const maxWait = 30 * time.Second
	for {
		nextWake := s.processDue()
		wait := time.Until(nextWake)
		if wait < time.Second {
			wait = time.Second
		}
		if wait > maxWait {
			wait = maxWait
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-s.tick:
		}
	}
}

// processDue scans the retry dir, moves any due files back into the inbox,
// and returns the time the earliest still-pending file is due (used to size
// the next sleep).
func (s *spool) processDue() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	nextWake := now.Add(30 * time.Second)

	entries, err := os.ReadDir(s.cfg.RetryDir())
	if err != nil {
		return nextWake
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".retry.json") {
			continue
		}
		sidecarPath := filepath.Join(s.cfg.RetryDir(), e.Name())
		data, err := os.ReadFile(sidecarPath)
		if err != nil {
			continue
		}
		var rec retryRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			slog.Warn("retry_sidecar_unreadable", "file", e.Name(), "err", err)
			continue
		}
		if rec.NextAttemptAt.After(now) {
			if rec.NextAttemptAt.Before(nextWake) {
				nextWake = rec.NextAttemptAt
			}
			continue
		}

		notePath := filepath.Join(s.cfg.RetryDir(), rec.BaseName)
		if _, err := os.Stat(notePath); err != nil {
			// File missing but sidecar present — clean up the sidecar.
			_ = os.Remove(sidecarPath)
			continue
		}
		inboxPath := filepath.Join(s.cfg.InboxDir(), rec.BaseName)
		if err := os.Rename(notePath, inboxPath); err != nil {
			slog.Warn("retry_requeue_move_failed", "file", rec.BaseName, "err", err)
			continue
		}
		if err := os.Remove(sidecarPath); err != nil {
			slog.Warn("retry_sidecar_cleanup_failed", "sidecar", sidecarPath, "err", err)
		}
		slog.Info("retry_requeue", "file", rec.BaseName, "attempt", rec.AttemptCount,
			"waited_s", int(time.Since(rec.FirstQueuedAt).Seconds()))
	}
	return nextWake
}
