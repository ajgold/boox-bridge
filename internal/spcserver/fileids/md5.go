package fileids

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// MD5For returns the lowercase MD5 hex digest of the file at absPath. The device
// wants MD5 in EntriesVO.content_hash / UserFileVO.md5, while UB's notes table
// stores SHA-256 — so this is a separate, lazily-computed digest. The result is
// cached in the file's spc_file_ids row keyed on (size, mtime); a later call
// with an unchanged size+mtime serves the cache without re-reading the bytes,
// and any size or mtime change forces a recompute. The path must already have an
// id (call IDFor first, as the mapping layer does); if it doesn't, one is
// assigned here so MD5For is safe to call standalone.
func (r *Registry) MD5For(ctx context.Context, absPath string) (string, error) {
	id, err := r.IDFor(ctx, absPath)
	if err != nil {
		return "", err
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("fileids MD5For stat %q: %w", absPath, err)
	}
	size := fi.Size()
	mtime := fi.ModTime().UnixMilli()

	var (
		cached   string
		cachedSz int64
		cachedMt int64
	)
	if err := r.db.QueryRowContext(ctx,
		`SELECT md5, md5_size, md5_mtime FROM spc_file_ids WHERE id = ?`, id,
	).Scan(&cached, &cachedSz, &cachedMt); err != nil {
		return "", fmt.Errorf("fileids MD5For read cache %d: %w", id, err)
	}
	if cached != "" && cachedSz == size && cachedMt == mtime {
		return cached, nil
	}

	digest, err := hashFile(absPath)
	if err != nil {
		return "", err
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE spc_file_ids SET md5 = ?, md5_size = ?, md5_mtime = ? WHERE id = ?`,
		digest, size, mtime, id,
	); err != nil {
		return "", fmt.Errorf("fileids MD5For write cache %d: %w", id, err)
	}
	return digest, nil
}

func hashFile(absPath string) (string, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return "", fmt.Errorf("fileids hash open %q: %w", absPath, err)
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("fileids hash read %q: %w", absPath, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
