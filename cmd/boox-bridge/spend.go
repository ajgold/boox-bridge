package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Approximate Claude API pricing in USD per million tokens (2026 estimates).
// Used only for the daily cost cap — actual billing happens at Anthropic.
// Reviewed: 2026-05. Adjust if pricing changes.
var modelPricing = map[string]struct {
	InputPerM  float64
	OutputPerM float64
}{
	"claude-sonnet-4-6": {InputPerM: 3.00, OutputPerM: 15.00},
	"claude-opus-4-7":   {InputPerM: 15.00, OutputPerM: 75.00},
	"claude-haiku-4-5":  {InputPerM: 0.80, OutputPerM: 4.00},
}

// spend tracks rough daily Claude spend so a sync-loop bug can't generate
// an unbounded bill.
type spend struct {
	mu        sync.Mutex
	path      string
	cap       float64
	tz        *time.Location
	totals    map[string]float64 // YYYY-MM-DD -> USD
}

func openSpend(stateDir string, cap float64) (*spend, error) {
	tz, err := time.LoadLocation("Australia/Brisbane")
	if err != nil {
		tz = time.UTC
	}
	s := &spend{
		path:   filepath.Join(stateDir, "spend.json"),
		cap:    cap,
		tz:     tz,
		totals: make(map[string]float64),
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.totals); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *spend) day() string {
	return time.Now().In(s.tz).Format("2006-01-02")
}

// remaining returns USD left for today.
func (s *spend) remaining() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cap - s.totals[s.day()]
}

// charge records a call's cost and returns an error if the cap is busted
// (call still recorded so we can see the overage).
func (s *spend) charge(model string, inputTokens, outputTokens int) (float64, error) {
	p, ok := modelPricing[model]
	if !ok {
		p = modelPricing["claude-sonnet-4-6"] // conservative fallback
	}
	cost := float64(inputTokens)*p.InputPerM/1_000_000 + float64(outputTokens)*p.OutputPerM/1_000_000

	s.mu.Lock()
	defer s.mu.Unlock()
	day := s.day()
	s.totals[day] += cost
	total := s.totals[day]
	if err := s.flushLocked(); err != nil {
		return cost, err
	}
	if total > s.cap {
		return cost, fmt.Errorf("daily cap exceeded: $%.4f > $%.2f", total, s.cap)
	}
	return cost, nil
}

func (s *spend) flushLocked() error {
	b, err := json.MarshalIndent(s.totals, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
