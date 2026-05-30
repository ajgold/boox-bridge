# Task Store

Last verified: 2026-05-29 (Task model gained CreatedAt + ForestNote* fields)

## Purpose
CRUD access to Supernote's `t_schedule_task` table with field mapping between
Go types, CalDAV VTODO properties, and the Supernote DB schema.

## Contracts
- **Exposes**: `Store` (List, Get, Create, Update, Delete, MaxLastModified), `Task` model, mapping helpers (GenerateTaskID, ComputeETag, ComputeCTag, CalDAVStatus, SupernoteStatus, MsToTime, TimeToMs, SqlStr, NullStr, CompletionTime)
- **Guarantees**: All queries scoped to a single user_id. List/Get exclude soft-deleted rows. Create sets defaults for missing fields. Update always bumps last_modified. Delete is soft-delete.
- **Expects**: Valid `*sql.DB` connected to supernotedb. Single user_id from `db.DiscoverUserID`.

## Dependencies
- **Uses**: `database/sql` only (no other internal packages)
- **Used by**: `caldav.Backend`, `web.Handler` (both via `caldav.TaskStore` interface)
- **Boundary**: No HTTP, no iCal -- pure data layer

## Key Decisions
- Sort columns omitted: Task model skips 8 sort columns; device repopulates on sync
- MD5 task IDs: `GenerateTaskID` uses MD5(title+timestamp) to match device convention
- ETag from mutable fields: title + status + due_time + last_modified

## Invariants
- Timestamps are always millisecond UTC unix (0 = unset)
- `completed_time` holds **creation** time (Supernote quirk); `last_modified` holds actual completion time
- `is_deleted` is "Y" or "N", never NULL
- `is_reminder_on` defaults to "N"
- `status` is "needsAction" or "completed" (Supernote values, not CalDAV casing)
- `ical_blob` (ICalBlob field) is optional and NULL for tasks from Supernote; populated by CalDAV write path for round-trip VTODO fidelity
- `CreatedAt` (int64 ms UTC) and the four `ForestNote*` nullable strings are **taskdb-only** — they mirror columns that only exist in UB's `tasks` table. The SPC mapping layer (`internal/spcserver/mapping`) leaves them at zero / `sql.NullString{}` and the device-side `t_schedule_task` schema is unchanged.

## Gotchas
- `ErrNotFound` sentinel: use `taskstore.IsNotFound(err)` to check, not type assertion
- CompletionTime reads from last_modified, NOT completed_time
- The REST API's `created_at` field is sourced from `Task.CreatedAt`; prior to 2026-05-29 it was mis-mapped from `DueTime`. Code reading the old field via `mapInternalTask` must use the new column.
