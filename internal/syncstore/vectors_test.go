package syncstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// vectorsDir is the shared conformance suite (docs/sync/vectors/), resolved
// relative to this package dir (internal/syncstore → repo root). The SAME JSON
// is run by the ForestNote Kotlin client; neither side is the source of truth.
const vectorsDir = "../../docs/sync/vectors"

type vector struct {
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Ops           []Op            `json:"ops"`
	ExpectedState map[string][]Op `json:"expected_state"` // table -> rows (rows are winning ops sans `table`)
}

// TestConformanceVectors runs every docs/sync/vectors/*.vector.json through the
// pure Merge and asserts the materialized winners equal expected_state. This is
// the contract test: a failure here means the Go merge diverges from the spec
// (and therefore, potentially, from the Kotlin client).
func TestConformanceVectors(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(vectorsDir, "*.vector.json"))
	if err != nil {
		t.Fatalf("glob vectors: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no vectors found in %s", vectorsDir)
	}

	for _, p := range paths {
		p := p
		t.Run(filepath.Base(p), func(t *testing.T) {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			var v vector
			if err := json.Unmarshal(data, &v); err != nil {
				t.Fatalf("parse %s: %v", p, err)
			}

			got := Merge(v.Ops)

			// Build the expected winner map, stamping each row with its table
			// (the table is implied by the array it lives in) and normalizing
			// cols so the comparison ignores any incidental unknown keys.
			want := make(map[TablePK]Op)
			for table, rows := range v.ExpectedState {
				for _, r := range rows {
					r.Table = table
					want[TablePK{Table: table, PK: r.PK}] = Normalize(r)
				}
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("vector %q diverged\n got: %s\nwant: %s",
					v.Name, mustJSON(got), mustJSON(want))
			}
		})
	}
}

func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
