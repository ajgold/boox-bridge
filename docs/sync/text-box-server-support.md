# Server support for ForestNote text boxes

**Status:** spec, not yet implemented. Written 2026-05-27 against `main` (post-pull, the forestnote
source/render/pdf/inventory subsystem).
**Client side:** built and committed on the ForestNote branch `feat/text-boxes` (schema v10,
migration `9.sqm`), with all sync plumbing present but **gated off** (`TEXT_BOX_SYNC_ENABLED=false`)
until this server work lands. Client plan: `~/ForestNote/.claude/plans/synchronous-jumping-petal.md`.

## What a text box is

A new syncable row type on the ForestNote side: a z-ordered text element living on a page, beside
`stroke`. One new table, `text_box`, full-row-upsert + tombstone, merged by the same row-level LWW
rule as every other table. The client stores geometry and font size in **virtual units** (page
short axis = 10,000), text as a plain UTF-8 column, font as a `/system/fonts` basename, and a `z`
band (0 = below ink, 1 = above ink).

### Authoritative column set (must match the client byte-for-byte)

The synced `cols` (the PK `id` is carried out-of-band as `pk`, never in `cols` — same as `stroke`):

| col | type | notes |
|---|---|---|
| `page_id` | string (ULID) | parent page pk |
| `x` | int64 | left, virtual units (short axis = 10,000) |
| `y` | int64 | top, virtual units |
| `width` | int64 | virtual units |
| `height` | int64 | virtual units (box auto-grows downward on the client) |
| `text` | string | the content — **the searchable / server-mutable payload** |
| `font_name` | string | a tablet `/system/fonts` basename, e.g. `Roboto-Regular.ttf`; `""` = system default |
| `font_size` | int64 | virtual units |
| `color` | int64 | signed ARGB sent as **unsigned int64** (same convention as `stroke.color`) |
| `weight` | int64 | 400 = normal, 700 = bold |
| `border_width` | int64 | screen px; 0 = no border, 2 = hairline |
| `z` | int64 | band: 0 = below ink, 1 = above ink |
| `created_at` | int64 ms UTC | |
| `deleted_at` | int64 ms UTC \| null | `null` = live (tombstone-as-column) |

**Canonical column order is alphabetical** (the server's `canonicalSchema()` requires it, and the
client's `SyncMerge.knownCols["text_box"]` is already written alphabetically to match):

```
border_width, color, created_at, deleted_at, font_name, font_size, height,
page_id, text, weight, width, x, y, z
```

---

## The big picture

UltraBridge is **not a relay-only sync endpoint** — it materializes a SQLite mirror, renders each
page to an image/PDF, OCRs the ink, and full-text-indexes the result. So "support text boxes" is
four jobs, in dependency order:

1. **Accept the sync** — mirror the rows (required; without it, clients 409 or silently lose data).
2. **Bump the schema hash** — the coordinated cutover with the client.
3. **Render** the boxes into the page image/PDF.
4. **Index** the box text so it's searchable.

Only (1)+(2) are required to avoid breaking sync. (3) and (4) are what make the feature pay off.
A fifth concern — **server-authored mutation of box text** — overlaps with the separate
"server-side operations on data" work; see the last section.

---

## Part A — Accept the sync (required)

### A1. `internal/syncstore/op.go:27-35` — declare the table

Add to `knownCols` (alphabetical columns, identical to the client list above) and append
`"text_box"` to `tableOrder`:

```go
var knownCols = map[string][]string{
    ...
    "stroke":   {"color","created_at","deleted_at","page_id","pen_width_max","pen_width_min","points","z"},
    "text_box": {"border_width","color","created_at","deleted_at","font_name","font_size","height","page_id","text","weight","width","x","y","z"},
}

var tableOrder = []string{"folder", "notebook", "page", "stroke", "text_box"}
```

This **automatically** changes `SchemaHash()` — `canonicalSchema()` (op.go:40) builds
`table:col,col;…` and `SchemaHash()` (op.go:51) is its SHA-256. No hash is hand-edited in
production code; it is derived. (Confirmed: op.go:40-54.)

### A2. `internal/syncstore/schema.go` — mirror table

In `Migrate()`, after the `fn_stroke` block (~schema.go:80), add `fn_text_box` mirroring the
stroke table's shape: all 14 value columns + the provenance trio
(`lww_wall_ts INTEGER NOT NULL`, `lww_op_seq INTEGER NOT NULL`, `lww_site_id TEXT NOT NULL`),
`id TEXT PRIMARY KEY`. Then an index:

```sql
CREATE TABLE IF NOT EXISTS fn_text_box (
    id            TEXT PRIMARY KEY,
    page_id       TEXT,
    x             INTEGER, y INTEGER, width INTEGER, height INTEGER,
    text          TEXT,
    font_name     TEXT,
    font_size     INTEGER,
    color         INTEGER,
    weight        INTEGER,
    border_width  INTEGER,
    z             INTEGER,
    created_at    INTEGER,
    deleted_at    INTEGER,
    lww_wall_ts   INTEGER NOT NULL,
    lww_op_seq    INTEGER NOT NULL,
    lww_site_id   TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_fn_text_box_page ON fn_text_box(page_id, z);
```

`Migrate()` is `CREATE TABLE IF NOT EXISTS`, so this is non-destructive and forward-only.

### A3. `internal/syncstore/store.go` — materialize on apply

`mergeRow` (store.go:192) switches on `n.Table` (store.go:211). Add:

```go
case "text_box":
    pagePK, err = upsertTextBox(ctx, tx, n)
```

and a new `upsertTextBox(ctx, tx, n Op) (pageID string, err error)` modeled on `upsertStroke`
(store.go:328): `INSERT … ON CONFLICT(id) DO UPDATE SET …` over the 14 columns + provenance,
reading values from `n.Cols` via the existing typed `Cols` accessors (store.go:431/440/458).
**Return the row's `page_id`** as `pagePK` — that is what the bridge keys re-render + re-index on.

> Without this case, ops pass `knownCols` validation, the hash matches, the changelog records them
> for relay — but they **never materialize in the mirror** (silent). This is the nastiest failure
> mode; don't skip it.

Note the `color` round-trip: it arrives as an unsigned int64 on the wire and is stored as the
signed value, exactly as `stroke.color` is handled today — reuse that path.

### A4. The changelog + gate are generic — no change

- `store.go:109-125` assigns the next global `seq` and appends the op to `sync_ops` verbatim for
  relay; this is table-agnostic, so `text_box` ops relay to other devices for free once A1–A3 land.
- `store.go:41-43` rejects unknown tables (`"unknown table"`); A1 makes `text_box` known.
- `internal/syncsvc/service.go:82` is the 409 gate; A1 makes the new hash the accepted one.

---

## Part B — Schema-hash bump + cutover (coordinated, atomic)

The hash gates *all* sync for a device (one mismatch → 409, device can't sync at all). So:

1. Land A1–A3 **in one server release**. `SchemaHash()` now returns the v2 value.
2. **Read the new hash from the test.** Update `internal/syncstore/op_test.go:8`
   (`schemaHashV1` constant) — run the test, copy the actual SHA from the failure, that *is* the
   value. (There is no other place the production hash is written; it's derived.)
3. Put that same SHA into the **client** constant `SyncProtocol.SCHEMA_HASH`
   (`~/ForestNote/core/sync/.../SyncProtocol.kt`), and flip the client
   `NotebookRepository.TEXT_BOX_SYNC_ENABLED` to `true`. Ship the client.
4. Until step 3, clients run v1 (no text_box ops) and still match the server **only if the server
   still advertises v1** — so do **not** deploy the server bump to production until you're ready for
   v1 clients to be told to update. (If old v1 clients must keep working during rollout, you need
   the server to accept *both* hashes transitionally — see "Open question" below.)

### Open question for your server-side-ops planning
`SchemaHash()` returns a single value and `service.go:82` compares for exact equality, so the
current design is **hard cutover**: the instant the server advertises v2, every un-updated v1 client
409s. If you want a grace window, you'll want the gate to accept a *set* of known-good hashes
(v1 ∪ v2) during migration. That's a small change at `service.go:82` + `op.go`, and it generalizes
to every future schema bump — worth folding into the broader plan.

---

## Part C — Conformance vectors (the shared contract)

`internal/syncstore/vectors_test.go` auto-discovers `docs/sync/vectors/*.vector.json`. Add:

- `NN-text-box-basic.vector.json` — one `text_box` op materializes to a live row with provenance.
- `NN-text-box-delete.vector.json` — a tombstone (`deleted_at != null`) wins by LWW and drops the
  row from live reads; a later un-delete restores it.
- `NN-text-box-lww.vector.json` — two ops on the same pk converge to the greater
  `(wall_ts, op_seq, site_id)`.

**These vectors are canonical here and mirrored *into* the client** (`ForestNote`
`core/format/src/test/resources/sync-vectors/`). They were deliberately *not* added on the client
yet — write them here first, then mirror. Also update the human contract:
`docs/sync/forestnote-sync-protocol.md` (the "three/four tables" prose, the §3.1 column table, and
the §6 canonical-string + hash).

---

## Part D — Render text boxes into the page image/PDF

Rendering is decoupled: ops → mirror → `syncbridge` re-renders the changed page →
`forestrender` draws an image → `forestpdf` staples images into a PDF. `forestpdf/pdf.go` needs
**no change** (it assembles pre-rendered JPEGs).

### D1. `internal/syncstore/reads.go` — read accessor
Add `LivePageTextBoxes(ctx, pagePK) ([]TextBoxData, error)` + a `TextBoxData` struct, parallel to
`LivePageStrokes` (reads.go:37): `… WHERE page_id = ? AND deleted_at IS NULL ORDER BY z`.

### D2. `internal/forestrender/render.go` — draw them
Add a `TextBox` type and extend `RenderPage` to take boxes alongside strokes. Draw with the `gg`
lib (already imported), wrapping `text` to `width`. **Two gotchas:**
- **Virtual units.** `x/y/width/height/font_size` are virtual (short axis 10,000). Apply the same
  virtual→pixel scale the stroke path already uses; do **not** treat them as pixels.
- **Fonts.** `font_name` is a *tablet* font basename the server won't have. Map name → an available
  server face with a default fallback (mirror the client's `FontCatalog.resolve` fallback). Ship at
  least one bundled face so absent fonts still render.
- **Z bands.** Draw `z==0` boxes beneath the ink, `z==1` boxes above it (the client renders
  template → bottom boxes → ink → top boxes; match that order). Draw the border rect when
  `border_width > 0`. Clip text to the box rect (overflow is retained in data, not drawn) to match
  the client.

### D3. `internal/syncbridge/bridge.go` (~141-161) — wire it
After `LivePageStrokes`, also fetch `LivePageTextBoxes`, add a `MapTextBoxes` (parallel to
`MapStrokes`, ~bridge.go:207), and pass both into `forestrender.RenderPage`.

### D4. `internal/service/note.go` `renderForestNotePage` (~529) — same wiring
Fetch text boxes and pass them to the render call here too.

---

## Part E — Make the box text searchable

The bridge OCRs ink and indexes the text via `internal/search/index.go` (FTS5). Box text is
*native* — no OCR, higher quality.

**`internal/syncbridge/bridge.go` (~163-176):** after fetching live boxes (D3), concatenate their
`.Text` (newline-joined) and merge into the page's indexed body **before** `IndexPage`. That's the
whole job for v1 — `search/index.go` indexes whatever text it's handed, so **no search-schema
change is needed**.

Future option (only if you want to distinguish a "matched in handwriting (OCR)" hit from a
"matched in a text box" hit): a separate `note_text_box_fts` table. Not required now.

---

## Part F — No change needed

- `internal/fnpath/fnpath.go` — a box lives inside a page; the page URI already addresses it.
- `internal/source/forestnote/source.go` / `config.go` — generic; drives the bridge, not
  column-aware.
- `internal/syncstore/inventory.go` — folder/notebook/page hierarchy for the Files tab (but see the
  next section — its mutation path is the overlap).
- `internal/forestpdf/pdf.go` — assembles pre-rendered images; text is already baked in.

---

## Part G — Overlap: server-authored mutation of box text

This is the part to fold into your "permit server-side operations on data" work — text boxes are
the ideal first mutation target because `text` is a plain string and a full-row upsert is trivial.

**What exists today (`internal/syncstore/inventory.go:218-267`):** the server already mutates the
mirror locally — e.g. it `UPDATE fn_stroke/fn_page/fn_notebook SET deleted_at = ?,
lww_site_id = 'ub-web'` to stamp UB-local provenance. **But its own comment is the key constraint:**

> "because two-way push isn't built, the delete is never relayed, so a subsequent [pull doesn't
> carry it back to devices]."

So a server-side edit today changes only the **mirror**, and devices never see it. Two gaps to close
for a server-authored text edit to round-trip back to the tablet:

1. **Author into the changelog, not just the mirror.** A relayable mutation must go through the same
   path client ops take: allocate the next global `seq` (store.go:113), build the full-row `cols`
   payload, run `mergeRow` to materialize, and `INSERT INTO sync_ops` (store.go:122) so it's pulled
   by devices. The inventory delete path writes the mirror directly and skips this — fine for a
   local-only effect, insufficient for relay.

2. **A server site identity + op sequence.** Relayed ops carry `(wall_ts, op_seq, site_id)` and
   `site_id` must be a **ULID** (`store.go:49` rejects non-ULID `site_id`). The existing local writes
   use the sentinel `'ub-web'`, which is *not* a ULID — so it works for direct mirror writes but
   **cannot be used for a relayable op as-is**. A server-authored op needs either (a) a real
   ULID-shaped server site id with its own monotonic `op_seq` counter, or (b) a deliberate relaxation
   of the ULID check for a reserved server site. Decide this once; it applies to *all* server-side
   mutations, not just text boxes.

**For text boxes specifically**, once (1)+(2) exist, a server edit is: build a `text_box` upsert op
with the new `text` (and a fresh `wall_ts`, the server site id, the next server `op_seq`), reusing
the same `cols` shape defined in Part A — no new merge logic. The LWW rule then resolves
server-vs-device edits deterministically, same as device-vs-device.

**Recommendation:** design the server-authoring primitive generically (author-op-for-(table, pk,
cols)) as part of the server-side-ops feature; text-box text edit becomes a thin caller of it.

---

## Failure modes (what breaks if you skip a step)

- Skip A1 (`knownCols`): client 409s on the hash → **can't sync at all**; or if the hash matched
  some other way, ops hit `"unknown table"` (store.go:43) and are permanently rejected.
- Skip A3 (`mergeRow` case): ops accepted, hash matches, changelog records them — but boxes
  **never materialize** in the mirror. Silent. Worst case.
- Do A only, skip D/E: sync round-trips device↔device, but boxes are **invisible in PDFs** and
  **unsearchable** server-side.
- Bump the hash in prod before clients update, with no multi-hash grace window: every v1 client
  **409s until updated** (see Part B open question).

---

## Suggested rollout order

1. **A + C** in one release (mirror accepts + materializes + vectors pass). Read the new hash from
   the failing `op_test.go`.
2. Update the client `SCHEMA_HASH`, flip `TEXT_BOX_SYNC_ENABLED`, ship the client. Verify a box
   round-trips into `fn_text_box`.
3. **D** (render) — boxes appear in PDFs/page images.
4. **E** (index) — box text becomes searchable.
5. **G** (server-authored edits) — folded into the server-side-operations feature; text-box text
   edit is the first concrete caller.

## File checklist

Accept sync: `op.go` · `schema.go` · `store.go` (+ `upsertTextBox`) · `op_test.go` (hash) ·
`docs/sync/forestnote-sync-protocol.md` · `docs/sync/vectors/*.vector.json`
Render: `reads.go` · `forestrender/render.go` · `syncbridge/bridge.go` · `service/note.go`
Search: `syncbridge/bridge.go`
Server-authored (overlap): `store.go` (author-op path) + a server site-id/op-seq mechanism + the
`site_id` ULID-validation decision (`store.go:49`)
