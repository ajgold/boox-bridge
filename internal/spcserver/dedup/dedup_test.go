package dedup

import (
	"testing"
	"time"
)

// TestSeenDeduplicates verifies first call is false, an immediate identical
// repeat is true, and after the TTL it is false again.
// Verifies: spc-phase-1.AC4.4
func TestSeenDeduplicates(t *testing.T) {
	base := time.Now()
	c := NewChecker()
	c.now = func() time.Time { return base }

	if c.Seen("u1", "/task", []byte(`{"a":1}`)) {
		t.Errorf("first Seen should be false")
	}
	if !c.Seen("u1", "/task", []byte(`{"a":1}`)) {
		t.Errorf("immediate repeat should be true (deduplicated)")
	}

	c.now = func() time.Time { return base.Add(2 * time.Second) } // TTL is 1s
	if c.Seen("u1", "/task", []byte(`{"a":1}`)) {
		t.Errorf("after TTL the same request should be false again")
	}
}

// TestSeenIndependentKeys verifies different body/endpoint/user are independent.
func TestSeenIndependentKeys(t *testing.T) {
	c := NewChecker()
	c.Seen("u1", "/task", []byte(`{"a":1}`)) // record

	if c.Seen("u1", "/task", []byte(`{"a":2}`)) {
		t.Errorf("different body must not dedup")
	}
	if c.Seen("u1", "/other", []byte(`{"a":1}`)) {
		t.Errorf("different endpoint must not dedup")
	}
	if c.Seen("u2", "/task", []byte(`{"a":1}`)) {
		t.Errorf("different user must not dedup")
	}
}
