package tasksync

// pattern: Imperative Shell

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sysop/ultrabridge/internal/taskstore"
)

// TaskStore is the subset of caldav.TaskStore needed by the sync engine.
type TaskStore interface {
	List(ctx context.Context) ([]taskstore.Task, error)
	Get(ctx context.Context, taskID string) (*taskstore.Task, error)
	Create(ctx context.Context, t *taskstore.Task) error
	Update(ctx context.Context, t *taskstore.Task) error
	Delete(ctx context.Context, taskID string) error
}

// SyncEngine manages adapter registration and reconciliation cycles.
type SyncEngine struct {
	store    TaskStore
	syncMap  *SyncMap
	logger   *slog.Logger
	interval time.Duration

	mu             sync.Mutex
	adapter        DeviceAdapter
	adapterStarted bool
	cancel         context.CancelFunc
	done           chan struct{}
	trigger        chan struct{}
	status         SyncStatus
}

// NewSyncEngine creates a sync engine.
func NewSyncEngine(store TaskStore, db *sql.DB, logger *slog.Logger, interval time.Duration) *SyncEngine {
	return &SyncEngine{
		store:    store,
		syncMap:  NewSyncMap(db),
		logger:   logger,
		interval: interval,
		trigger:  make(chan struct{}, 1),
	}
}

// RegisterAdapter sets the active adapter. Must be called before Start.
func (e *SyncEngine) RegisterAdapter(adapter DeviceAdapter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.adapter = adapter
	e.status.AdapterID = adapter.ID()
	e.status.AdapterActive = true
}

// UnregisterAdapter removes the active adapter.
func (e *SyncEngine) UnregisterAdapter() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.adapter != nil {
		e.adapter.Stop()
		e.adapter = nil
	}
	e.status.AdapterActive = false
}

// Start begins the background sync loop. Follows processor pattern.
//
// adapter.Start() failure is non-fatal: the run loop still launches and
// retries adapter init at the top of each cycle. This keeps the engine
// resilient to cold-start races where the remote endpoint (e.g. SPC over
// Docker DNS) isn't reachable at the exact moment the binary boots.
func (e *SyncEngine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		return fmt.Errorf("sync engine already running")
	}
	if e.adapter == nil {
		return fmt.Errorf("no adapter registered")
	}
	if err := e.adapter.Start(ctx); err != nil {
		e.logger.Warn("adapter start failed, will retry in sync loop", "adapter", e.adapter.ID(), "error", err)
		e.status.LastError = fmt.Sprintf("adapter start: %s", err)
	} else {
		e.adapterStarted = true
	}
	ctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.done = make(chan struct{})
	go e.run(ctx)
	return nil
}

// Stop halts the sync loop and waits for completion.
func (e *SyncEngine) Stop() error {
	e.mu.Lock()
	cancel := e.cancel
	done := e.done
	e.cancel = nil
	e.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	<-done
	if e.adapter != nil {
		e.adapter.Stop()
	}
	return nil
}

// TriggerSync requests an immediate sync cycle.
func (e *SyncEngine) TriggerSync() {
	select {
	case e.trigger <- struct{}{}:
	default:
		// Already triggered, don't block
	}
}

// Status returns the current sync status.
func (e *SyncEngine) Status() SyncStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

func (e *SyncEngine) run(ctx context.Context) {
	defer close(e.done)
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.runCycle(ctx)
		case <-e.trigger:
			e.runCycle(ctx)
			ticker.Reset(e.interval)
		}
	}
}

func (e *SyncEngine) runCycle(ctx context.Context) {
	e.mu.Lock()
	adapter := e.adapter
	started := e.adapterStarted
	e.status.InProgress = true
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.status.InProgress = false
		e.mu.Unlock()
	}()

	if adapter == nil {
		return
	}

	if !started {
		if err := adapter.Start(ctx); err != nil {
			e.mu.Lock()
			e.status.LastError = fmt.Sprintf("adapter start: %s", err)
			e.mu.Unlock()
			e.logger.Warn("adapter start retry failed", "adapter", adapter.ID(), "error", err)
			return
		}
		e.mu.Lock()
		e.adapterStarted = true
		e.mu.Unlock()
		e.logger.Info("adapter started on retry", "adapter", adapter.ID())
	}

	err := e.reconcile(ctx, adapter)

	e.mu.Lock()
	now := time.Now().UnixMilli()
	e.status.LastSyncAt = now
	e.status.NextSyncAt = now + e.interval.Milliseconds()
	if err != nil {
		e.status.LastError = err.Error()
		e.logger.Warn("sync cycle failed", "adapter", adapter.ID(), "error", err)
	} else {
		e.status.LastError = ""
		e.logger.Info("sync cycle completed", "adapter", adapter.ID())
	}
	e.mu.Unlock()
}

func (e *SyncEngine) reconcile(ctx context.Context, adapter DeviceAdapter) error {
	adapterID := adapter.ID()

	// 1. Get sync token
	token, err := e.syncMap.GetSyncToken(ctx, adapterID)
	if err != nil {
		return fmt.Errorf("get sync token: %w", err)
	}

	// 2. Pull remote tasks
	remoteTasks, newToken, err := adapter.Pull(ctx, token)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}

	// 3. Process remote changes (imports, updates, deletes)
	for _, rt := range remoteTasks {
		if err := e.processRemoteTask(ctx, adapterID, rt); err != nil {
			e.logger.Warn("process remote task failed", "remote_id", rt.RemoteID, "error", err)
		}
	}

	// 4. Detect remote hard-deletes: sync_map entries whose remote_id is
	//    not in the Pull results represent tasks deleted on the remote side.
	remoteIDs := make(map[string]bool, len(remoteTasks))
	for _, rt := range remoteTasks {
		remoteIDs[rt.RemoteID] = true
	}
	entries, err := e.syncMap.ListByAdapter(ctx, adapterID)
	if err != nil {
		return fmt.Errorf("list sync map: %w", err)
	}
	for _, entry := range entries {
		if entry.RemoteID == "" {
			continue // Never pushed — skip hard-delete detection
		}
		if entry.LastSeen == 0 {
			continue // Never observed in a Pull — task may not have propagated
			// yet, or the remote silently rejected our push. Either way, give
			// it grace; don't soft-delete prematurely.
		}
		if !remoteIDs[entry.RemoteID] {
			// Remote task gone — soft-delete locally
			if err := e.store.Delete(ctx, entry.TaskID); err != nil {
				e.logger.Warn("soft-delete for remote hard-delete failed", "task_id", entry.TaskID, "error", err)
			}
			if err := e.syncMap.DeleteByTaskID(ctx, entry.TaskID, adapterID); err != nil {
				e.logger.Warn("delete sync map entry failed", "task_id", entry.TaskID, "error", err)
			}
			e.logger.Info("remote hard-delete detected, soft-deleted locally", "task_id", entry.TaskID, "remote_id", entry.RemoteID)
		}
	}

	// 5. Find local changes to push
	changes, err := e.findLocalChanges(ctx, adapterID)
	if err != nil {
		return fmt.Errorf("find local changes: %w", err)
	}

	// 6. Push local changes. The adapter contract states that PushResult is
	//    only emitted for changes that the remote actually confirmed; any
	//    change absent from results was rejected (e.g., SPC returned
	//    success=false). We must only update the sync map for confirmed
	//    items, otherwise we accumulate phantom entries that will never be
	//    cleaned up by the hard-delete detector.
	if len(changes) > 0 {
		results, err := adapter.Push(ctx, changes)
		if err != nil {
			return fmt.Errorf("push: %w", err)
		}
		now := time.Now().UnixMilli()
		resultByTaskID := make(map[string]PushResult, len(results))
		for _, r := range results {
			resultByTaskID[r.TaskID] = r
		}
		changeByTaskID := make(map[string]Change, len(changes))
		for _, c := range changes {
			changeByTaskID[c.TaskID] = c
		}
		for taskID, r := range resultByTaskID {
			c, ok := changeByTaskID[taskID]
			if !ok {
				continue
			}
			if c.Type == ChangeDelete {
				if err := e.syncMap.DeleteByTaskID(ctx, c.TaskID, adapterID); err != nil {
					e.logger.Warn("delete sync map after push failed", "task_id", c.TaskID, "error", err)
				}
				continue
			}
			remoteID := c.RemoteID
			if r.RemoteID != "" {
				remoteID = r.RemoteID
			}
			remoteETag := ""
			lastPulled := int64(0)
			lastSeen := int64(0)
			entry, _ := e.syncMap.GetByTaskID(ctx, c.TaskID, adapterID)
			if entry != nil {
				remoteETag = entry.RemoteETag
				lastPulled = entry.LastPulled
				lastSeen = entry.LastSeen
			}
			if err := e.syncMap.Upsert(ctx, &SyncMapEntry{
				TaskID:     c.TaskID,
				AdapterID:  adapterID,
				RemoteID:   remoteID,
				RemoteETag: remoteETag,
				LastPushed: now,
				LastPulled: lastPulled,
				LastSeen:   lastSeen,
			}); err != nil {
				e.logger.Warn("upsert sync map after push failed", "task_id", c.TaskID, "error", err)
			}
		}
	}

	// 7. Update sync token
	now := time.Now().UnixMilli()
	if err := e.syncMap.SetSyncToken(ctx, adapterID, newToken, now); err != nil {
		return fmt.Errorf("set sync token: %w", err)
	}

	return nil
}

func (e *SyncEngine) processRemoteTask(ctx context.Context, adapterID string, rt RemoteTask) error {
	entry, err := e.syncMap.GetByRemoteID(ctx, adapterID, rt.RemoteID)
	if err != nil {
		return err
	}

	if entry == nil {
		// New remote task — import to local store
		e.logger.Info("importing new remote task", "remote_id", rt.RemoteID, "title", rt.Title)
		t := remoteToLocal(rt)
		if err := e.store.Create(ctx, t); err != nil {
			return fmt.Errorf("create imported task: %w", err)
		}
		now := time.Now().UnixMilli()
		return e.syncMap.Upsert(ctx, &SyncMapEntry{
			TaskID:     t.TaskID,
			AdapterID:  adapterID,
			RemoteID:   rt.RemoteID,
			RemoteETag: rt.ETag,
			LastPushed: 0,
			LastPulled: now,
			LastSeen:   now,
		})
	}

	// Existing task — check if remote changed
	if entry.RemoteETag == rt.ETag {
		// Remote unchanged. Bump LastSeen as proof-of-life so the
		// hard-delete detector knows we've observed the entry, but DO NOT
		// bump LastPulled — there is no remote change to apply, and bumping
		// LastPulled would mask any unpushed local edit from
		// findLocalChanges (which uses max(LastPushed, LastPulled) as the
		// "synced as of" cutoff).
		now := time.Now().UnixMilli()
		return e.syncMap.Upsert(ctx, &SyncMapEntry{
			TaskID:     entry.TaskID,
			AdapterID:  adapterID,
			RemoteID:   rt.RemoteID,
			RemoteETag: rt.ETag,
			LastPushed: entry.LastPushed,
			LastPulled: entry.LastPulled,
			LastSeen:   now,
		})
	}

	// Remote changed — check if local also changed (conflict)
	local, err := e.store.Get(ctx, entry.TaskID)
	if err != nil {
		if taskstore.IsNotFound(err) {
			// Local was deleted — soft-delete wins (UB-wins)
			return nil
		}
		return err
	}

	localModified := int64(0)
	if local.LastModified.Valid {
		localModified = local.LastModified.Int64
	}

	if localModified > maxInt64(entry.LastPushed, entry.LastPulled) {
		// Both sides changed — UB wins, will be pushed back in step 6
		e.logger.Info("conflict: UB wins", "task_id", entry.TaskID, "adapter", adapterID)
		return nil
	}

	// Only remote changed — apply remote update
	applyRemoteToLocal(local, rt)
	if err := e.store.Update(ctx, local); err != nil {
		return fmt.Errorf("update from remote: %w", err)
	}
	now := time.Now().UnixMilli()
	return e.syncMap.Upsert(ctx, &SyncMapEntry{
		TaskID:     entry.TaskID,
		AdapterID:  adapterID,
		RemoteID:   rt.RemoteID,
		RemoteETag: rt.ETag,
		LastPushed: entry.LastPushed,
		LastPulled: now,
		LastSeen:   now,
	})
}

func (e *SyncEngine) findLocalChanges(ctx context.Context, adapterID string) ([]Change, error) {
	tasks, err := e.store.List(ctx)
	if err != nil {
		return nil, err
	}

	entries, err := e.syncMap.ListByAdapter(ctx, adapterID)
	if err != nil {
		return nil, err
	}

	// Build lookup: taskID -> sync map entry
	entryByTaskID := make(map[string]*SyncMapEntry, len(entries))
	for i := range entries {
		entryByTaskID[entries[i].TaskID] = &entries[i]
	}

	var changes []Change
	for i := range tasks {
		t := &tasks[i]
		entry := entryByTaskID[t.TaskID]

		if entry == nil {
			// Local task not yet synced — create on remote
			changes = append(changes, Change{
				Type:   ChangeCreate,
				TaskID: t.TaskID,
				Remote: localToRemote(t),
			})
			continue
		}

		// Check if local changed since last push or pull
		localModified := int64(0)
		if t.LastModified.Valid {
			localModified = t.LastModified.Int64
		}
		
		e.logger.Info("findLocalChanges debug", "task_id", t.TaskID, "local_mod", localModified, "last_pushed", entry.LastPushed, "last_pulled", entry.LastPulled, "remote_id", entry.RemoteID)

		if localModified > maxInt64(entry.LastPushed, entry.LastPulled) {
			e.logger.Info("findLocalChanges appending update", "task_id", t.TaskID)
			changes = append(changes, Change{
				Type:     ChangeUpdate,
				TaskID:   t.TaskID,
				RemoteID: entry.RemoteID,
				Remote:   localToRemote(t),
			})
		}

		delete(entryByTaskID, t.TaskID)
	}

	// Remaining entries are tasks deleted locally (not in List because soft-deleted)
	for _, entry := range entryByTaskID {
		changes = append(changes, Change{
			Type:     ChangeDelete,
			TaskID:   entry.TaskID,
			RemoteID: entry.RemoteID,
		})
	}

	return changes, nil
}

// remoteToLocal converts a RemoteTask to a new local Task.
func remoteToLocal(rt RemoteTask) *taskstore.Task {
	now := time.Now().UnixMilli()
	ct := rt.CompletedTime
	if ct == 0 {
		ct = now
	}
	return &taskstore.Task{
		TaskID:        taskstore.GenerateTaskID(rt.Title, now),
		Title:         taskstore.SqlStr(rt.Title),
		Detail:        taskstore.SqlStr(rt.Detail),
		Status:        taskstore.SqlStr(rt.Status),
		Importance:    taskstore.SqlStr(rt.Importance),
		DueTime:       rt.DueTime,
		CompletedTime: sql.NullInt64{Int64: ct, Valid: true},
		LastModified:  sql.NullInt64{Int64: now, Valid: true},
		Recurrence:    taskstore.SqlStr(rt.Recurrence),
		IsReminderOn:  rt.IsReminderOn,
		Links:         taskstore.SqlStr(rt.Links),
		IsDeleted:     "N",
	}
}

// applyRemoteToLocal updates a local task with remote field values.
func applyRemoteToLocal(local *taskstore.Task, rt RemoteTask) {
	local.Title = taskstore.SqlStr(rt.Title)
	local.Detail = taskstore.SqlStr(rt.Detail)
	local.Status = taskstore.SqlStr(rt.Status)
	local.Importance = taskstore.SqlStr(rt.Importance)
	local.DueTime = rt.DueTime
	if rt.CompletedTime != 0 {
		local.CompletedTime = sql.NullInt64{Int64: rt.CompletedTime, Valid: true}
	}
	local.Recurrence = taskstore.SqlStr(rt.Recurrence)
	local.IsReminderOn = rt.IsReminderOn
	local.Links = taskstore.SqlStr(rt.Links)
}

// maxInt64 returns the maximum of two int64 values.
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// localToRemote converts a local Task to a RemoteTask for pushing.
func localToRemote(t *taskstore.Task) RemoteTask {
	return RemoteTask{
		Title:         taskstore.NullStr(t.Title),
		Detail:        taskstore.NullStr(t.Detail),
		Status:        taskstore.NullStr(t.Status),
		Importance:    taskstore.NullStr(t.Importance),
		DueTime:       t.DueTime,
		CompletedTime: t.CompletedTime.Int64,
		Recurrence:    taskstore.NullStr(t.Recurrence),
		IsReminderOn:  t.IsReminderOn,
		Links:         taskstore.NullStr(t.Links),
	}
}
