# Server support for ForestNote page text (`page_text_from_server`)

**Status:** IMPLEMENTED on branch `feat/forestnote-page-text-from-server` (schema v3). Written
2026-05-27 against `main` (post text-box server-authoring + two-way push).
**Client side:** see `docs/sync/page-text-client-handoff.md` — the matching client steps
(migration `11.sqm`, schema v11→v12, `SCHEMA_HASH` → v3). The client is a read-only consumer.

## What this is

UltraBridge already OCRs every changed ForestNote page (`internal/syncbridge/bridge.go`) and
stores the text in its private FTS index (`note_content`, keyed `forestnote://{nb}/{page}`).
That text never reached the tablet. This feature pushes the recognized text **down to the
device** over the existing `/sync/v1` changelog so the client can persist (and later search) it.

Two new synced tables, identical shape, **pk == the page ULID** (1:1 with `page`):

- **`page_text_from_server`** — authored **only by UB** (the OCR result). This is what ships.
- **`page_text_from_client`** — **reserved** for a future on-device-recognition feature. The
  table + apply/merge path exist on both sides now (so its columns are baked into the v3 hash
  and adopting client-authoring needs no second bump), but **nothing authors it** in v3.

**Why a separate table, not a column on `page`:** the protocol is full-row-upsert + LWW. If the
text lived on `fn_page`, any device-authored page op (reorder/template/delete) would carry an
empty `text` and clobber the server's OCR by recency. A dedicated **single-writer** table the
device never authors has no cross-writer hazard. The device MUST NOT author either table.

### Authoritative column set (matches the client byte-for-byte)

Synced `cols`, **alphabetical** (the PK is carried as `pk`, never in `cols`):

| col | type | notes |
|---|---|---|
| `created_at` | int64 ms UTC | first recognition time |
| `deleted_at` | int64 ms UTC \| null | `null` = live; set when the page is deleted/cleared |
| `model` | string \| null | recognizer model / source; `null` = unspecified |
| `ocr_at` | int64 ms UTC | last recognition time |
| `text` | string | the recognized text (flat, per-page; `""` allowed) |

## What was built

### Accept the sync + bump the hash (`internal/syncstore/`)
- **`op.go`** — `knownCols` gains `page_text_from_server` and `page_text_from_client` (both
  `{created_at, deleted_at, model, ocr_at, text}`); `tableOrder` inserts them alphabetically
  between `page` and `stroke`. `SchemaHash()` is derived from this, so it auto-advances to v3.
  `schemaHashV2` is promoted to a frozen literal; `AcceptsSchemaHash` admits `{v3, v2}` (v1
  retired). **v3 = `724411eb845ad3487393a77cb5559690e69332c35fdb5ee3e85c1767bf71f3fe`.**
- **`schema.go`** — `fn_page_text_from_server` + `fn_page_text_from_client` mirror tables
  (`id PK`, 5 value cols, provenance trio). `CREATE TABLE IF NOT EXISTS`; no secondary index
  (only lookup is by PK, which is the page id).
- **`store.go`** — `upsertPageText(ctx, tx, n)` materializes into `fn_<table>`; the `mergeRow`
  switch handles both tables and **returns no `pagePK`** — load-bearing: a page_text op must not
  enter `ChangedPages` or the bridge would re-OCR→re-author in a loop.

### Author the text (`internal/syncstore/pagetext.go`)
- `AuthorPageText(ctx, pageID, text, ocrAt, model)` and `AuthorPageTextTombstone(ctx, pageID)`
  build full-row `page_text_from_server` ops (all 5 cols present — `validateOp` requires it) and
  go through `AuthorOps`, so they are recorded under UB's site_id and relayed on the next pull.
- `inventory.go` `SoftDeleteNotebook` cascades a page_text tombstone for each deleted page.

### Produce the text (`internal/syncbridge/bridge.go`)
- After the page body is assembled and indexed, `processPage` calls `AuthorPageText` (or, for an
  empty body, `AuthorPageTextTombstone`). `dropPage` (deleted/blank page) tombstones too. All
  best-effort (warn-on-error). `model` is `""` (the narrow OCR interface exposes none in v1).

### Backfill (`internal/source/forestnote/backfill.go`)
- `backfillPageText` reads existing `note_content` `forestnote://` rows and authors a
  page_text op per page that lacks one. Launched as a one-shot goroutine at the end of
  `Source.Start` (after `Migrate`). Idempotent: skips pages that already have a row, so it only
  fills the pre-feature gap and a restart re-authors nothing.

## Rollout (coordinated, server-first)

1. **Server (this branch)** ships advertising v3 and accepting `{v3, v2}`. v2 clients keep
   syncing and silently ignore relayed `page_text_*` ops (spec §3.2). The backfill runs once.
2. **Client** ships with `SCHEMA_HASH = v3`, the `fn_page_text_from_server` table, and the
   read-only apply path; on first pull it receives current + backfilled text without re-OCR.
3. **Later** a server change drops v2 from the accepted set.

> v1 (pre-text_box) is **not** accepted — its grace window closed with the text_box rollout.

## Conformance vectors (shared contract, `docs/sync/vectors/`)

- `23-page-text-basic` — one op → one live row (all 5 cols + provenance).
- `24-page-text-tombstone` — a newer op tombstones the row (cleared/deleted page).
- `25-page-text-reocr` — a newer non-delete op overwrites `text`; `created_at` carries forward.

Canonical here; **mirrored verbatim into the client** (`core/format/src/test/resources/sync-vectors/`).

## Verification

- `go test ./internal/syncstore/ ./internal/syncbridge/ ./internal/source/forestnote/` — vectors
  23/24/25 pass, `SchemaHash()==v3`, grace window admits `{v2,v3}` (rejects v1), author/tombstone/
  re-OCR LWW + loop-safety (no changed pages) covered.
- `python3 docs/sync/vectors/_oracle.py docs/sync/vectors/*.vector.json` — all PASS.
- Live: trigger OCR (sync a stroke change), then
  `sqlite3 <notedb> "SELECT id, substr(text,1,60), ocr_at, deleted_at FROM fn_page_text_from_server LIMIT 5;"`
  and confirm a `page_text_from_server` op appears in `sync_ops`.
