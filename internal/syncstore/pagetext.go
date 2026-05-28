package syncstore

import (
	"context"
	"fmt"
	"time"
)

// pagetext.go authors the server-owned page_text_from_server entity: UB's OCR result
// for a page, pushed down to the device over the changelog so the client can persist
// (and later search) it. pk == the page ULID (1:1 with the page), so re-OCR re-authors
// the SAME row and the device converges by LWW. The device treats this table as
// read-only (it never authors page_text_* ops), so there is no cross-writer hazard —
// see forestnote-sync-protocol.md §3 and docs/sync/page-text-server-support.md.

// pageTextCols builds the full 5-column cols map for a page_text_from_server op. Every
// known column must be present (validateOp rejects a missing one), even on a tombstone.
// Numbers are float64 because an authored op must look exactly like a decoded device op
// (JSON numbers arrive as float64); model is a string or nil (nullable column).
func pageTextCols(text string, ocrAt, createdAt int64, model string, deletedAt *int64) map[string]any {
	var modelVal any
	if model != "" {
		modelVal = model
	}
	var delVal any
	if deletedAt != nil {
		delVal = float64(*deletedAt)
	}
	return map[string]any{
		"created_at": float64(createdAt),
		"deleted_at": delVal,
		"model":      modelVal,
		"ocr_at":     float64(ocrAt),
		"text":       text,
	}
}

// AuthorPageText authors (or re-authors) the recognized text for a page as a
// page_text_from_server upsert through AuthorOps — recorded under UB's site_id and
// relayed to the user's devices on their next pull. ocrAt stamps both created_at and
// ocr_at (first/last recognition coincide for a fresh author; the column split lets a
// future caller preserve an earlier created_at). model is the recognizer/source, "" → null.
func (s *Store) AuthorPageText(ctx context.Context, pageID, text string, ocrAt int64, model string) error {
	if !IsULID(pageID) {
		return fmt.Errorf("page id is not a ULID: %s", pageID)
	}
	op := Op{Table: "page_text_from_server", PK: pageID, Cols: pageTextCols(text, ocrAt, ocrAt, model, nil)}
	if _, err := s.AuthorOps(ctx, []Op{op}); err != nil {
		return fmt.Errorf("author page text: %w", err)
	}
	return nil
}

// AuthorPageTextTombstone tombstones a page's recognized-text row (deleted_at = now) so a
// deleted/cleared page stops carrying stale text to devices. All five columns are present
// because validateOp requires it; the content columns are blanked.
func (s *Store) AuthorPageTextTombstone(ctx context.Context, pageID string) error {
	if !IsULID(pageID) {
		return fmt.Errorf("page id is not a ULID: %s", pageID)
	}
	now := time.Now().UnixMilli()
	op := Op{Table: "page_text_from_server", PK: pageID, Cols: pageTextCols("", 0, now, "", &now)}
	if _, err := s.AuthorOps(ctx, []Op{op}); err != nil {
		return fmt.Errorf("author page text tombstone: %w", err)
	}
	return nil
}
