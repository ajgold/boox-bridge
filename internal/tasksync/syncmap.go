package tasksync

// pattern: Imperative Shell

import (
	"context"
	"database/sql"
	"fmt"
)

// SyncMap provides data access for sync_state and task_sync_map tables.
type SyncMap struct {
	db *sql.DB
}

// NewSyncMap creates a sync map accessor.
func NewSyncMap(db *sql.DB) *SyncMap {
	return &SyncMap{db: db}
}

// SyncMapEntry represents a row in task_sync_map.
//
// LastPushed: last time we pushed local state to remote.
// LastPulled: last time we applied a remote change to local. NOT bumped on
//   no-op (ETag-match) pulls — bumping it would mask unpushed local edits
//   from findLocalChanges.
// LastSeen:   last time the remote_id was observed in any pull (including
//   ETag-match). Used solely for hard-delete detection.
type SyncMapEntry struct {
	TaskID     string
	AdapterID  string
	RemoteID   string
	RemoteETag string
	LastPushed int64
	LastPulled int64
	LastSeen   int64
}

// GetSyncToken returns the last sync token for an adapter.
func (m *SyncMap) GetSyncToken(ctx context.Context, adapterID string) (string, error) {
	var token sql.NullString
	err := m.db.QueryRowContext(ctx,
		"SELECT last_sync_token FROM sync_state WHERE adapter_id = ?",
		adapterID).Scan(&token)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get sync token: %w", err)
	}
	if !token.Valid {
		return "", nil
	}
	return token.String, nil
}

// SetSyncToken upserts the sync token and timestamp for an adapter.
func (m *SyncMap) SetSyncToken(ctx context.Context, adapterID, token string, syncAt int64) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO sync_state (adapter_id, last_sync_token, last_sync_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(adapter_id) DO UPDATE SET
		   last_sync_token = excluded.last_sync_token,
		   last_sync_at = excluded.last_sync_at`,
		adapterID, token, syncAt)
	if err != nil {
		return fmt.Errorf("set sync token: %w", err)
	}
	return nil
}

// GetByTaskID returns the sync map entry for a task+adapter pair.
func (m *SyncMap) GetByTaskID(ctx context.Context, taskID, adapterID string) (*SyncMapEntry, error) {
	row := m.db.QueryRowContext(ctx,
		`SELECT task_id, adapter_id, remote_id, remote_etag, last_pushed_at, last_pulled_at, last_seen_at
		 FROM task_sync_map WHERE task_id = ? AND adapter_id = ?`,
		taskID, adapterID)
	var e SyncMapEntry
	err := row.Scan(&e.TaskID, &e.AdapterID, &e.RemoteID, &e.RemoteETag, &e.LastPushed, &e.LastPulled, &e.LastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sync map entry: %w", err)
	}
	return &e, nil
}

// GetByRemoteID returns the sync map entry for a remote_id+adapter pair.
func (m *SyncMap) GetByRemoteID(ctx context.Context, adapterID, remoteID string) (*SyncMapEntry, error) {
	row := m.db.QueryRowContext(ctx,
		`SELECT task_id, adapter_id, remote_id, remote_etag, last_pushed_at, last_pulled_at, last_seen_at
		 FROM task_sync_map WHERE adapter_id = ? AND remote_id = ?`,
		adapterID, remoteID)
	var e SyncMapEntry
	err := row.Scan(&e.TaskID, &e.AdapterID, &e.RemoteID, &e.RemoteETag, &e.LastPushed, &e.LastPulled, &e.LastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sync map by remote ID: %w", err)
	}
	return &e, nil
}

// ListByAdapter returns all sync map entries for a given adapter.
func (m *SyncMap) ListByAdapter(ctx context.Context, adapterID string) ([]SyncMapEntry, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT task_id, adapter_id, remote_id, remote_etag, last_pushed_at, last_pulled_at, last_seen_at
		 FROM task_sync_map WHERE adapter_id = ?`,
		adapterID)
	if err != nil {
		return nil, fmt.Errorf("list sync map: %w", err)
	}
	defer rows.Close()
	var entries []SyncMapEntry
	for rows.Next() {
		var e SyncMapEntry
		if err := rows.Scan(&e.TaskID, &e.AdapterID, &e.RemoteID, &e.RemoteETag, &e.LastPushed, &e.LastPulled, &e.LastSeen); err != nil {
			return nil, fmt.Errorf("scan sync map: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Upsert creates or updates a sync map entry.
func (m *SyncMap) Upsert(ctx context.Context, e *SyncMapEntry) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO task_sync_map (task_id, adapter_id, remote_id, remote_etag, last_pushed_at, last_pulled_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(task_id, adapter_id) DO UPDATE SET
		   remote_id = excluded.remote_id,
		   remote_etag = excluded.remote_etag,
		   last_pushed_at = excluded.last_pushed_at,
		   last_pulled_at = excluded.last_pulled_at,
		   last_seen_at = excluded.last_seen_at`,
		e.TaskID, e.AdapterID, e.RemoteID, e.RemoteETag, e.LastPushed, e.LastPulled, e.LastSeen)
	if err != nil {
		return fmt.Errorf("upsert sync map: %w", err)
	}
	return nil
}

// DeleteByTaskID removes the sync map entry for a task+adapter pair.
func (m *SyncMap) DeleteByTaskID(ctx context.Context, taskID, adapterID string) error {
	_, err := m.db.ExecContext(ctx,
		"DELETE FROM task_sync_map WHERE task_id = ? AND adapter_id = ?",
		taskID, adapterID)
	if err != nil {
		return fmt.Errorf("delete sync map: %w", err)
	}
	return nil
}
