package staging

import (
	"context"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
)

// TestMigrateIdempotent verifies Migrate can run twice without error and that
// the spc_uploads table is usable afterward (insert + read a row).
func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO spc_uploads(inner_name, target_path, file_name, claimed_md5, claimed_size, status, created_at, expires_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		"inner-1", "/Note", "foo.note", "deadbeef", 1234, statusApplied, 1000, 2000); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var (
		target string
		size   int64
		status string
	)
	if err := db.QueryRowContext(ctx,
		`SELECT target_path, claimed_size, status FROM spc_uploads WHERE inner_name = ?`, "inner-1").
		Scan(&target, &size, &status); err != nil {
		t.Fatalf("select: %v", err)
	}
	if target != "/Note" || size != 1234 || status != statusApplied {
		t.Fatalf("round-trip mismatch: target=%q size=%d status=%q", target, size, status)
	}
}
