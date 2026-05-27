# Dev note: ForestNote two-way push (UB as an authoring sync site)

Status: **not built** — design/handoff note for a future session. Written 2026-05-27
right after the ForestNote Files-tab parity work (PR #33) landed on `main`.

## Why this note exists

The new Files tab has a **Delete** action that soft-deletes a notebook UB-side and
de-indexes it. It is honest but one-directional: ForestNote is a
**device-authoritative** source and UB cannot push anything back to the device, so
a deleted-in-UB notebook **reappears** if the device still has it and the user edits
it again (a newer `wall_ts` wins LWW and the bridge re-indexes it). Re-OCR and
future edit/control surfaces have the same ceiling. "Two-way push" is the missing
capability. Goal of the next session: make UB a first-class authoring participant in
the sync log so server-originated changes reach the device.

## The key insight (don't re-derive this)

The sync **protocol is already bidirectional**. `/sync/v1`
(`internal/syncsvc/service.go:78`, `Sync`) does, per request:

1. `store.ApplyBatch(siteID, ops)` — merge the **device's** pushed ops into the mirror.
2. `store.OpsSince(cursor, excludeSite=siteID, limit)` — **relay** ops back to the device.

`OpsSince` (`internal/syncstore/store.go`) is:
```sql
SELECT seq, payload FROM sync_ops WHERE seq > ? AND site_id <> ? ORDER BY seq
```
It returns ops authored by **some other site**. So a device already pulls changes
that originated elsewhere. The wire needs **no change** to carry UB-authored ops.

**What's missing:** the *only* writer to `sync_ops` is `ApplyBatch`, and it only
records ops a **device** sent. **UB never authors an op.** It is a passive
mirror+relay. `SoftDeleteNotebook` (`internal/syncstore/inventory.go`) does a direct
`UPDATE` on the `fn_*` tables (sets `deleted_at`, bumps `lww_wall_ts`, stamps
`lww_site_id='ub-web'`) and deliberately does **not** touch `sync_ops`. Therefore the
relay can't see it and the device never learns. Bumping `lww_wall_ts` only helps the
delete beat *replays of older* device ops; a *newer* device edit still wins.

## What "build two-way push" means

Make UB author real ops into the changelog so the existing relay carries them out:

1. **Give UB a site identity.** A persistent `site_id` that is a **valid ULID**
   (`validateOp` in `store.go` requires `IsULID(op.SiteID)`; the device client almost
   certainly validates incoming op site_ids too — so `'ub-web'` is NOT acceptable on
   the wire, it was only ever a local-provenance marker). Generate once, persist
   (settings table or a dedicated `sync_site` row), reuse across restarts.
2. **Give UB a per-site op_seq counter.** Monotonic, starting at 1, persisted and
   incremented per authored op (mirror how devices number their ops). Spec requires
   `op_seq > 0` and uses `(site_id, op_seq)` for dedup.
3. **Author ops, don't mutate the mirror directly.** Add a store method (e.g.
   `AuthorOps(ctx, ops []Op) error`) that, in one tx: for each op, `Normalize` →
   `mergeRow` (so the local mirror updates via the same LWW path) → bump the global
   `sync_seq` (`UPDATE sync_seq ... RETURNING last_seq`) → `INSERT INTO sync_ops`
   with UB's `site_id`/`op_seq`/`wall_ts` and the JSON payload. This is essentially
   the back half of `ApplyBatch`'s loop (`store.go` ~lines 100-130) refactored to be
   callable for server-originated ops. Reuse, don't duplicate.
4. **Rewire the UB-side actions to author instead of UPDATE.** `SoftDeleteNotebook`
   becomes "author delete ops" (set `deleted_at`) for the notebook + its pages +
   strokes. The relay then carries them; on the device's next pull they apply via the
   device's own LWW and the notebook is deleted there too.

The relay/cursor machinery needs nothing new: `OpsSince` excludes only the *requesting*
site, so every device (including the one that originally created the notebook) pulls
UB's ops; `advanceAccepted`/`sync_cursors` are per-requesting-device and unaffected.

## Hard questions to settle (the real work is here, not the plumbing)

- **Conflict semantics.** Once both UB and the device can author, LWW
  (`reconcile.go:18`, `Less` on `(wall_ts, op_seq, site_id)`) decides. Is that what we
  want for a UB delete racing a device edit? Probably yes (last writer wins), but be
  deliberate and write conformance vectors for it (`docs/sync/vectors/`,
  `_oracle.py`).
- **Clock authority for `wall_ts`.** UB stamps `time.Now()`. The spec rejects ops
  whose `wall_ts` is far in the future relative to server time
  (`forestnote-sync-protocol.md` §clock-skew). UB *is* the server, so its own clock is
  the reference — fine, but make sure UB-authored ops aren't accidentally run through
  the skew guard.
- **Scope for v1 of two-way.** Recommend **delete-only push** first (the concrete
  need), then generalize to edits/renames and the "control surfaces" idea. Keep the
  first PR small and prove the round trip on hardware before widening.
- **Idempotency/replay.** UB's `(site_id, op_seq)` must never be reused; persist the
  counter transactionally with the op insert so a crash can't double-allocate or skip.
- **Spec + vectors.** Update `docs/sync/forestnote-sync-protocol.md` to describe the
  server as an authoring site, and add conformance vectors for server-authored
  delete + the UB-vs-device conflict cases.

## Files to start from

- `internal/syncstore/store.go` — `ApplyBatch` (the loop to factor), `OpsSince` (relay,
  unchanged), `mergeRow`, `validateOp` (ULID/op_seq rules), `sync_seq`/`sync_cursors`.
- `internal/syncstore/inventory.go` — `SoftDeleteNotebook` (today's direct-UPDATE; the
  thing to convert to authored ops).
- `internal/syncsvc/service.go` — `Sync` (apply + relay); no change expected.
- `internal/syncstore/reconcile.go` — `Less` (the LWW rule that now arbitrates UB vs
  device).
- `internal/source/forestnote/source.go` — where UB's site identity / authoring hook
  would likely be wired (it already owns the store + bridge).
- `docs/sync/forestnote-sync-protocol.md` + `docs/sync/vectors/` — spec + conformance.

## How to verify (this session's setup)

- Running instance: `https://localhost:8443` (HTTP on :8443), Basic Auth
  `ultrabridge:ehh1701jqb` (disposable). ForestNote tab: `/files/forestnote`.
- Rebuild/restart after changes: `./rebuild.sh -y` (health-checks on its own).
- There are a few real synced notebooks to test against (e.g. "My Notebook",
  "20260527 Test"). A true round-trip test needs the actual device to pull and apply —
  capture-first on hardware, the same discipline the SPC phases used.
- Unit-test pattern for authored ops: assert a row lands in `sync_ops` with UB's
  `site_id` and that `OpsSince(cursor, excludeSite=<device>, ...)` returns it.

## Related memory

`project_forestnote_sync_v2_deferred` (forward backlog) and
`project_forestnote_ub_sync_design` (v1 device→server design, schema_hash, conformance
vectors). This note is the concrete "make UB an authoring site" slice of that backlog.
