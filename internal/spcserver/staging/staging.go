// Package staging owns the server-side half of the SPC upload flow: it accepts
// the device's uploaded bytes into a holding area under FILE_ROOT keyed by a
// server-chosen innerName, then (at upload/finish) verifies the claimed md5/size
// and atomically promotes the staged file to its human target path.
//
// Splitting the unverified byte-stream away from the real tree is deliberate: a
// device upload writes only to <FILE_ROOT>/.staging/<innerName> until its content
// hash is proven at finish, so a truncated/corrupt/forged upload never appears in
// the browsable tree. The spc_uploads table records the apply→finish correlation
// (innerName → target path + claimed md5/size) and a TTL for orphan cleanup.
//
// The table is migrated by this package (called from main in server mode), not
// by notedb.Open, keeping it gated to UB-as-SPC server mode (precedent:
// fileids.Migrate / mcpauth.Migrate).
package staging

import (
	"context"
	"database/sql"
	"fmt"
)

// Upload-row lifecycle states recorded in spc_uploads.status.
const (
	statusApplied   = "applied"   // apply issued; awaiting bytes + finish
	statusFinalized = "finalized" // verified + promoted to the target path
)

// Migrate creates the spc_uploads table idempotently.
func Migrate(ctx context.Context, db *sql.DB) error {
	const stmt = `CREATE TABLE IF NOT EXISTS spc_uploads (
		inner_name   TEXT PRIMARY KEY,
		target_path  TEXT NOT NULL,
		file_name    TEXT NOT NULL,
		claimed_md5  TEXT NOT NULL DEFAULT '',
		claimed_size INTEGER NOT NULL DEFAULT 0,
		status       TEXT NOT NULL DEFAULT 'applied',
		created_at   INTEGER NOT NULL DEFAULT 0,
		expires_at   INTEGER NOT NULL DEFAULT 0
	)`
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("staging migration: %w", err)
	}
	return nil
}
