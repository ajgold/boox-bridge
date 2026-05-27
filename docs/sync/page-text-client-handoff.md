# ForestNote client hand-off: `page_text_from_server` (OCR text round-trip)

Hand this to the ForestNote (Kotlin) Claude. It is the **client half** of the unified
plan; the UltraBridge server half is implemented in parallel. The two repos share one
wire contract — the values below are authoritative and must be matched byte-for-byte.

## What we're building (and why)

The UltraBridge server already OCRs every changed ForestNote page and keeps the text in
its own private index. The tablet never sees that text. This feature pushes the recognized
text **back down to the device** over the existing `/sync/v1` protocol so the client can
persist it locally (and, in a later plan, build on-device search).

**Scope of THIS plan:** data round-trip only — the text lands in a local table and persists,
verifiable by reading the on-device DB. **No** search UI, **no** local FTS yet, **no**
client-side recognition.

## The wire contract (must match the server exactly)

Two new synced entities, identical shape, **pk = page ULID** (1:1 with a page):

- **`page_text_from_server`** — server-authored ONLY. This plan consumes it.
- **`page_text_from_client`** — RESERVED for a future on-device-recognition feature.
  Create the table and apply path now (so we never need a second hash bump), but **nothing
  authors it yet** on either side.

Columns (this exact alphabetical order is what the schema hash is computed from):

| column       | type            | notes                                   |
|--------------|-----------------|-----------------------------------------|
| `created_at` | INTEGER NOT NULL| ms UTC, first recognition time          |
| `deleted_at` | INTEGER (null)  | nullable tombstone; null = live         |
| `model`      | TEXT (null)     | OCR/recognizer model or source          |
| `ocr_at`     | INTEGER NOT NULL| ms UTC, last recognition time           |
| `text`       | TEXT NOT NULL   | recognized text (flat, per-page; "" ok) |

Plus the usual provenance trio on the mirror row (`lww_wall_ts`, `lww_op_seq`, `lww_site_id`).

### Schema hash → **v3**

```
SCHEMA_HASH (v3) = 724411eb845ad3487393a77cb5559690e69332c35fdb5ee3e85c1767bf71f3fe
```

Canonical schema string it is computed from (tables alphabetical; the two new tables fall
between `page` and `stroke`; columns alphabetical within each table; `;`-joined, no spaces,
no trailing newline):

```
folder:created_at,deleted_at,name,parent_folder_id,sort_order;notebook:created_at,deleted_at,folder_id,name,sort_order;page:created_at,deleted_at,notebook_id,sort_order,template,template_pitch_mm;page_text_from_client:created_at,deleted_at,model,ocr_at,text;page_text_from_server:created_at,deleted_at,model,ocr_at,text;stroke:color,created_at,deleted_at,page_id,pen_width_max,pen_width_min,points,z;text_box:border_width,color,created_at,deleted_at,font_name,font_size,height,page_id,text,weight,width,x,y,z
```

`sha256(thatString)` == the v3 hash above. (Server reproduces the current v2 hash
`bc1953e2…` from the same procedure, so this derivation is verified.)

## The two load-bearing rules

1. **Never author `page_text_from_server` or `page_text_from_client`.** They are
   server-/future-authored. If the device ever emits an op for these tables (e.g. an empty
   `text` during a structural edit), LWW recency would clobber the server's text. The
   guarantee must be **structural**: zero `enqueueOp("page_text_from_*", …)` call sites.
2. **Ignore ops for unknown tables.** During the rollout grace window the v3 server relays
   these ops to not-yet-updated clients; they must be silently skipped, never rejected. (The
   text_box rollout already relied on this — confirm it still holds.)

## Client steps (ForestNote, Kotlin / SQLDelight)

- **C1. Migration `11.sqm` (v11→v12, additive):** `CREATE TABLE page_text_from_server (id
  TEXT PRIMARY KEY NOT NULL, text TEXT NOT NULL, ocr_at INTEGER NOT NULL, model TEXT,
  created_at INTEGER NOT NULL, deleted_at INTEGER);` and an identical `page_text_from_client`.
- **C2. `notebook.sq`:** add both `CREATE TABLE` after `text_box` (~line 88) for fresh
  installs; add `applyUpsertPageTextFromServer:` / `applyUpsertPageTextFromClient:`
  (INSERT … ON CONFLICT(id) DO UPDATE, all 5 cols), modeled on `applyUpsertTextBox` (~:540).
  **Omit** any send-side `allPageText*Ids`/`syncRow*` query — the client never authors these.
- **C3. `SyncWire.kt`:** `data class PageTextRow(text, ocrAt, model, createdAt, deletedAt)`;
  `decodePageText(cols)`; an encoder `pageTextCols(...)` only for `decode(encode())`
  round-trip test symmetry. Encoder key order alphabetical: `created_at, deleted_at, model,
  ocr_at, text`.
- **C4. `SyncMerge.kt` (~:21) knownCols:** add both tables →
  `listOf("created_at","deleted_at","model","ocr_at","text")`. Must match the server ordering.
- **C5. `NotebookRepository.writeWinningOp` (~:1057):** add `"page_text_from_server"` and
  `"page_text_from_client"` cases calling the respective `applyUpsert…`. Do **not** bump
  `notebook.modified_at` (not user-authored). `applySyncOps` (~:1040) already routes generically.
- **C6. Author-exclusion:** `buildCols` (~:967) returns null for both tables → `enqueueOp`
  no-op (comment it "server-authored / reserved"). No `allPageText*Ids` loop in
  `backfillOutbox` (~:868) or `rebackfillIfSchemaAdvanced` (~:891). Verify by grep: zero
  `enqueueOp("page_text_from_*", …)` sites.
- **C7. `SyncProtocol.kt:18`:** `SCHEMA_HASH` → the v3 value above. `PROTOCOL_VERSION` stays 1.
  Doc comment: grace set {v2, v3}.
- **C8. `SYNC_BACKFILL_VERSION` (`NotebookRepository.kt:101`):** **leave at 1** — it re-emits
  *client-authored* outbox ops, and these entities aren't client-authored. Comment it so no
  one "completes the pattern."
- **C9. Tests:** mirror conformance vectors `23`/`24`/`25` verbatim into
  `core/format/src/test/resources/sync-vectors/`; add a `SyncWireDecodeTest` page-text
  round-trip; migration test v11→v12 asserts both tables exist.

> Heads-up: `core/format/CLAUDE.md` looks stale (says `TEXT_BOX_SYNC_ENABLED=false`, schema
> v10) — the code is `true` / v11. Trust the code; refresh that doc opportunistically.

## Conformance vectors (the cross-language contract)

Three JSON vectors are the shared source of truth, produced in the server repo at
`docs/sync/vectors/` and run by BOTH sides' test harnesses:

- `23-page-text-basic.vector.json` — one `page_text_from_server` op → one live row (all 5 cols).
- `24-page-text-tombstone.vector.json` — LWW-newer delete wins (row converges to tombstoned).
- `25-page-text-reocr.vector.json` — same pk, LWW-newer non-delete op overwrites `text` (re-OCR).

Mirror these files verbatim once delivered (they'll be attached / committed server-side).

## Rollout ordering (one hard constraint)

A client may advertise hash **v3 only after the server accepts v3.** The server ships first
and accepts **{v3, v2}** during the grace window, so un-updated v2 clients keep syncing and
silently ignore the new ops (rule 2). Then the client ships with `SCHEMA_HASH = v3`. The
server also runs a one-time backfill of existing OCR text, so an updated client receives
historical page text on its next pull without any re-OCR. (Note: v1 — pre-text_box — is
**not** in the accepted set; any v1 client must already have upgraded.)

## How to verify on-device (the key proof)

After syncing a v3 client:
```
sqlite3 <app>/files/default.forestnote \
  "SELECT id, substr(text,1,80), ocr_at, model, deleted_at FROM page_text_from_server;"
```
Expect a row per synced OCR'd page. Delete a page → its row gets `deleted_at` stamped.
