package tasksync

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/taskdb"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// openTestDB creates an in-memory SQLite task database for testing.
func openTestDB(t *testing.T) *sql.DB {
	db, err := taskdb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("taskdb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// waitForSync waits for a sync cycle to complete after the given lastSyncAt.
// Pass engine.Status().LastSyncAt before triggering to wait for a NEW cycle.
func waitForSync(t *testing.T, engine *SyncEngine, lastSyncAt int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := engine.Status()
		if s.LastSyncAt > lastSyncAt && !s.InProgress {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sync did not complete within timeout")
}

// mockAdapter is a hand-rolled mock adapter for testing.
type mockAdapter struct {
	id        string
	tasks     map[string]RemoteTask
	startCnt  int
	startErrs []error // returned from Start by index; nil/out-of-range means success
	stopCnt   int
	pulls     []mockPull
	pushes    [][]Change
	nextToken string

	// failCreate, when true, simulates a silent remote rejection of Create
	// changes: the change is omitted from PushResult and the task is NOT
	// stored in m.tasks. Mirrors SPC returning success=false on /task POST.
	failCreate bool
}

type mockPull struct {
	since     string
	tasks     []RemoteTask
	nextToken string
	err       error
}

func newMockAdapter(id string) *mockAdapter {
	return &mockAdapter{
		id:    id,
		tasks: make(map[string]RemoteTask),
	}
}

func (m *mockAdapter) ID() string { return m.id }

func (m *mockAdapter) Start(ctx context.Context) error {
	idx := m.startCnt
	m.startCnt++
	if idx < len(m.startErrs) {
		return m.startErrs[idx]
	}
	return nil
}

func (m *mockAdapter) Stop() error {
	m.stopCnt++
	return nil
}

// Pull returns configured results or an error.
func (m *mockAdapter) Pull(ctx context.Context, since string) ([]RemoteTask, string, error) {
	for _, p := range m.pulls {
		if p.since == since {
			return p.tasks, p.nextToken, p.err
		}
	}
	// Default: return all tasks
	var tasks []RemoteTask
	for _, t := range m.tasks {
		tasks = append(tasks, t)
	}
	token := m.nextToken
	if token == "" {
		token = "token-v1"
	}
	return tasks, token, nil
}

// Push records changes and returns PushResults with server-assigned IDs if needed.
func (m *mockAdapter) Push(ctx context.Context, changes []Change) ([]PushResult, error) {
	m.pushes = append(m.pushes, changes)

	var results []PushResult
	for _, c := range changes {
		r := PushResult{TaskID: c.TaskID}
		if c.Type == ChangeCreate {
			if m.failCreate {
				continue // simulate silent rejection — no result, no stored task
			}
			// Simulate server assigning a remote ID
			r.RemoteID = "remote-" + c.TaskID[:8]
			stored := c.Remote
			stored.RemoteID = r.RemoteID
			m.tasks[r.RemoteID] = stored
		} else if c.Type == ChangeUpdate {
			r.RemoteID = c.RemoteID
			stored := c.Remote
			stored.RemoteID = c.RemoteID
			m.tasks[c.RemoteID] = stored
		} else if c.Type == ChangeDelete {
			delete(m.tasks, c.RemoteID)
		}
		results = append(results, r)
	}
	return results, nil
}

// setInitialTasks populates the mock adapter with tasks for Pull.
func (m *mockAdapter) setInitialTasks(tasks []RemoteTask) {
	m.tasks = make(map[string]RemoteTask)
	for _, t := range tasks {
		m.tasks[t.RemoteID] = t
	}
}

// TestSyncEngine_AC51_RegisterAndSync verifies AC5.1: Register mock adapter, start engine, trigger sync,
// Pull returns 2 tasks, both exist in local store after sync.
func TestSyncEngine_AC51_RegisterAndSync(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	adapter := newMockAdapter("test-adapter-1")
	remoteTask1 := RemoteTask{
		RemoteID: "remote-task-1",
		Title:    "Remote Task 1",
		Status:   "needsAction",
		ETag:     "etag-1",
	}
	remoteTask2 := RemoteTask{
		RemoteID: "remote-task-2",
		Title:    "Remote Task 2",
		Status:   "completed",
		ETag:     "etag-2",
	}
	adapter.setInitialTasks([]RemoteTask{remoteTask1, remoteTask2})

	engine.RegisterAdapter(adapter)

	ctx := context.Background()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	// Trigger sync
	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	// Verify both tasks exist in local store
	tasks, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List tasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}

	// Verify titles match
	titles := make(map[string]bool)
	for _, task := range tasks {
		titles[taskstore.NullStr(task.Title)] = true
	}
	if !titles["Remote Task 1"] || !titles["Remote Task 2"] {
		t.Errorf("expected tasks with titles 'Remote Task 1' and 'Remote Task 2', got %v", titles)
	}
}

// TestSyncEngine_AC51_CreateLocalAndPush verifies AC5.1: Create a local task, trigger sync.
// Mock adapter's Push receives ChangeCreate.
func TestSyncEngine_AC51_CreateLocalAndPush(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	adapter := newMockAdapter("test-adapter-2")
	engine.RegisterAdapter(adapter)

	ctx := context.Background()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	// Create a local task
	localTask := &taskstore.Task{
		Title:       taskstore.SqlStr("Local Task"),
		Status:      taskstore.SqlStr("needsAction"),
		IsDeleted:   "N",
		IsReminderOn: "N",
	}
	if err := store.Create(ctx, localTask); err != nil {
		t.Fatalf("Create task: %v", err)
	}

	// Trigger sync
	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	// Verify Push was called with ChangeCreate
	if len(adapter.pushes) != 1 {
		t.Fatalf("expected 1 push call, got %d", len(adapter.pushes))
	}
	changes := adapter.pushes[0]
	if len(changes) != 1 {
		t.Fatalf("expected 1 change in push, got %d", len(changes))
	}
	if changes[0].Type != ChangeCreate {
		t.Errorf("expected ChangeCreate, got %v", changes[0].Type)
	}
	if changes[0].Remote.Title != "Local Task" {
		t.Errorf("expected title 'Local Task', got %q", changes[0].Remote.Title)
	}
}

// TestSyncEngine_AC52_MultipleAdapters verifies AC5.2: Register/unregister adapter with no changes
// to task store. Register different adapter, verify it works.
func TestSyncEngine_AC52_MultipleAdapters(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	ctx := context.Background()

	// Create initial task
	task1 := &taskstore.Task{
		Title:       taskstore.SqlStr("Task 1"),
		Status:      taskstore.SqlStr("needsAction"),
		IsDeleted:   "N",
		IsReminderOn: "N",
	}
	if err := store.Create(ctx, task1); err != nil {
		t.Fatalf("Create task1: %v", err)
	}

	// Register first adapter
	adapter1 := newMockAdapter("adapter-1")
	engine.RegisterAdapter(adapter1)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start with adapter1: %v", err)
	}

	// Unregister and stop
	engine.Stop()
	engine.UnregisterAdapter()

	// Verify task store unchanged
	tasks, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 task after unregister, got %d", len(tasks))
	}

	// Register second adapter with different ID
	adapter2 := newMockAdapter("adapter-2")
	engine.RegisterAdapter(adapter2)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start with adapter2: %v", err)
	}
	defer engine.Stop()

	// Verify second adapter can sync without code changes
	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	// Status should reflect adapter2
	status := engine.Status()
	if status.AdapterID != "adapter-2" {
		t.Errorf("expected AdapterID='adapter-2', got %q", status.AdapterID)
	}
}

// TestSyncEngine_UBWinsConflict verifies UB-wins conflict resolution:
// Create local task, sync it (mock returns it), modify both local and remote.
// Local version should win (remote gets pushed the local version).
func TestSyncEngine_UBWinsConflict(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	ctx := context.Background()

	// Step 1: Create local task
	localTask := &taskstore.Task{
		Title:       taskstore.SqlStr("Task"),
		Status:      taskstore.SqlStr("needsAction"),
		IsDeleted:   "N",
		IsReminderOn: "N",
	}
	if err := store.Create(ctx, localTask); err != nil {
		t.Fatalf("Create task: %v", err)
	}
	taskID := localTask.TaskID

	// Step 2: First sync - task goes to remote
	adapter := newMockAdapter("test-adapter")
	engine.RegisterAdapter(adapter)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	// Get the remote ID from the sync map (set by engine after first sync)
	if len(adapter.pushes) == 0 {
		t.Fatalf("expected Push to be called")
	}
	if len(adapter.pushes[0]) == 0 {
		t.Fatalf("expected changes in Push")
	}
	// Step 3: Modify both local and remote
	localTask.Title = taskstore.SqlStr("Task - Local Edit")
	localTask.LastModified = sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}
	if err := store.Update(ctx, localTask); err != nil {
		t.Fatalf("Update task: %v", err)
	}

	remoteVersion := RemoteTask{
		RemoteID:  "remote-" + taskID[:8], // Use consistent remote ID
		Title:     "Task - Remote Edit",
		Status:    "completed",
		ETag:      "etag-conflict",
	}
	adapter.setInitialTasks([]RemoteTask{remoteVersion})

	// Clear previous push history
	adapter.pushes = nil

	// Step 4: Second sync - local should win
	beforeTs2 := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs2)

	// Verify local version is still in store
	current, err := store.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	if taskstore.NullStr(current.Title) != "Task - Local Edit" {
		t.Errorf("local edit lost, title is %q", taskstore.NullStr(current.Title))
	}

	// Verify local version was pushed to remote
	if len(adapter.pushes) == 0 || len(adapter.pushes[0]) == 0 {
		t.Errorf("expected Push to be called with local version after conflict")
	} else {
		change := adapter.pushes[0][0]
		if change.Type != ChangeUpdate || change.Remote.Title != "Task - Local Edit" {
			t.Errorf("expected ChangeUpdate with local title, got change type %v with title %q",
				change.Type, change.Remote.Title)
		}
	}
}

// TestSyncEngine_RemoteCreate verifies remote task import:
// Mock Pull returns a new task, after sync, task exists in local store with sync map entry.
func TestSyncEngine_RemoteCreate(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	ctx := context.Background()

	adapter := newMockAdapter("test-adapter")
	remoteTask := RemoteTask{
		RemoteID: "remote-123",
		Title:    "Imported Task",
		Status:   "needsAction",
		ETag:     "etag-orig",
	}
	adapter.setInitialTasks([]RemoteTask{remoteTask})

	engine.RegisterAdapter(adapter)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	// Verify task exists in local store
	tasks, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if taskstore.NullStr(tasks[0].Title) != "Imported Task" {
		t.Errorf("title mismatch: got %q", taskstore.NullStr(tasks[0].Title))
	}
}

// TestSyncEngine_RemoteUpdate verifies remote update:
// Mock Pull returns existing task with different ETag. After sync, local task updated (if local didn't change).
func TestSyncEngine_RemoteUpdate(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	ctx := context.Background()

	// Create local task
	localTask := &taskstore.Task{
		Title:       taskstore.SqlStr("Task"),
		Status:      taskstore.SqlStr("needsAction"),
		IsDeleted:   "N",
		IsReminderOn: "N",
	}
	if err := store.Create(ctx, localTask); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// First sync to establish sync map
	adapter := newMockAdapter("test-adapter")
	engine.RegisterAdapter(adapter)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	// Get remote ID from first sync (deterministic: "remote-" + taskID[:8])
	remoteID := "remote-" + localTask.TaskID[:8]

	// Update remote version with different content
	adapter.pushes = nil
	adapter.setInitialTasks([]RemoteTask{{
		RemoteID: remoteID,
		Title:    "Task - Remote Updated",
		Status:   "completed",
		ETag:     "etag-new",
	}})

	// Second sync
	beforeTs2 := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs2)

	// Verify local task was updated with remote content
	current, err := store.Get(ctx, localTask.TaskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if taskstore.NullStr(current.Title) != "Task - Remote Updated" {
		t.Errorf("expected title 'Task - Remote Updated', got %q", taskstore.NullStr(current.Title))
	}
	if taskstore.NullStr(current.Status) != "completed" {
		t.Errorf("expected status 'completed', got %q", taskstore.NullStr(current.Status))
	}
}

// TestSyncEngine_LocalDelete verifies local delete:
// Delete a synced task locally. Run sync. Verify ChangeDelete pushed to adapter.
func TestSyncEngine_LocalDelete(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	ctx := context.Background()

	// Create and sync a task
	localTask := &taskstore.Task{
		Title:       taskstore.SqlStr("Task to Delete"),
		Status:      taskstore.SqlStr("needsAction"),
		IsDeleted:   "N",
		IsReminderOn: "N",
	}
	if err := store.Create(ctx, localTask); err != nil {
		t.Fatalf("Create: %v", err)
	}

	adapter := newMockAdapter("test-adapter")
	engine.RegisterAdapter(adapter)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	// Delete local task
	if err := store.Delete(ctx, localTask.TaskID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	adapter.pushes = nil

	// Sync again
	beforeTs2 := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs2)

	// Verify ChangeDelete was pushed
	if len(adapter.pushes) == 0 {
		t.Fatal("expected Push to be called for delete")
	}
	if len(adapter.pushes[0]) == 0 {
		t.Fatal("expected changes in Push call")
	}
	change := adapter.pushes[0][0]
	if change.Type != ChangeDelete {
		t.Errorf("expected ChangeDelete, got %v", change.Type)
	}
}

// TestSyncEngine_RemoteHardDelete verifies AC2.9 (remote hard-delete):
// Create local task, sync it (mock returns it with sync_map entry). Next cycle,
// mock Pull returns empty. Verify local task is soft-deleted and sync_map entry removed.
func TestSyncEngine_RemoteHardDelete(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	ctx := context.Background()

	// Create local task
	localTask := &taskstore.Task{
		Title:       taskstore.SqlStr("Task"),
		Status:      taskstore.SqlStr("needsAction"),
		IsDeleted:   "N",
		IsReminderOn: "N",
	}
	if err := store.Create(ctx, localTask); err != nil {
		t.Fatalf("Create: %v", err)
	}
	taskID := localTask.TaskID

	// First sync - establish sync map
	adapter := newMockAdapter("test-adapter")
	engine.RegisterAdapter(adapter)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	// Intermediate sync - task is pulled, updating LastPulled>0
	beforeTsMid := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTsMid)

	// Verify task exists before remote delete
	task, err := store.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get before delete: %v", err)
	}
	if task.IsDeleted != "N" {
		t.Errorf("task should not be deleted before remote delete")
	}

	// Second sync - empty Pull (remote task deleted)
	adapter.pushes = nil
	adapter.setInitialTasks([]RemoteTask{}) // No tasks

	beforeTs2 := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs2)

	// Verify task is soft-deleted locally
	task, err = store.Get(ctx, taskID)
	if err != nil && !taskstore.IsNotFound(err) {
		t.Fatalf("Get after delete: %v", err)
	}
	if err == nil {
		t.Logf("Task still exists (soft-deleted): is_deleted=%q", task.IsDeleted)
	}

	// List should exclude soft-deleted
	tasks, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 active tasks after remote delete, got %d", len(tasks))
	}
}

// TestSyncEngine_StatusReporting verifies Status() reflects engine state:
// InProgress during cycle, LastSyncAt after, LastError on failure.
func TestSyncEngine_StatusReporting(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	ctx := context.Background()

	adapter := newMockAdapter("test-adapter")
	engine.RegisterAdapter(adapter)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	// Before sync
	status := engine.Status()
	if status.InProgress {
		t.Logf("Warning: InProgress true before sync trigger")
	}

	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	// After sync
	status = engine.Status()
	if !status.AdapterActive {
		t.Errorf("AdapterActive should be true")
	}
	if status.AdapterID != "test-adapter" {
		t.Errorf("AdapterID mismatch: got %q", status.AdapterID)
	}
	if status.LastSyncAt == 0 {
		t.Errorf("LastSyncAt should be set")
	}
	if status.LastError != "" {
		t.Errorf("LastError should be empty on success: %s", status.LastError)
	}
}

// TestSyncEngine_AdapterStartRetry verifies that when adapter.Start() fails
// at engine-start time (e.g. cold-start race against a remote service), the
// engine still launches its run loop, surfaces the failure via LastError,
// and retries adapter.Start() on the next cycle — succeeding once the
// remote becomes reachable.
func TestSyncEngine_AdapterStartRetry(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	adapter := newMockAdapter("retry-adapter")
	adapter.startErrs = []error{errors.New("dial tcp: lookup remote: no such host")}
	adapter.setInitialTasks([]RemoteTask{{
		RemoteID: "remote-1",
		Title:    "After retry",
		Status:   "needsAction",
		ETag:     "e1",
	}})
	engine.RegisterAdapter(adapter)

	ctx := context.Background()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("engine Start should not surface adapter start failure: %v", err)
	}
	defer engine.Stop()

	// First Start() attempt consumed the seeded error; LastError reflects it.
	if status := engine.Status(); !strings.Contains(status.LastError, "adapter start") {
		t.Errorf("expected LastError to mention adapter start failure, got %q", status.LastError)
	}
	if adapter.startCnt != 1 {
		t.Errorf("expected 1 Start call before retry, got %d", adapter.startCnt)
	}

	// Next cycle should retry Start (which now succeeds) and then reconcile.
	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	if adapter.startCnt != 2 {
		t.Errorf("expected Start to be retried (2 total calls), got %d", adapter.startCnt)
	}
	status := engine.Status()
	if status.LastError != "" {
		t.Errorf("LastError should be cleared after successful cycle, got %q", status.LastError)
	}

	// Verify reconcile actually ran by checking the task imported.
	tasks, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 1 || taskstore.NullStr(tasks[0].Title) != "After retry" {
		t.Errorf("expected imported task after retry, got %v", tasks)
	}
}

// TestSyncEngine_ManualTrigger verifies TriggerSync() causes immediate sync.
func TestSyncEngine_ManualTrigger(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 10*time.Second) // Long interval

	ctx := context.Background()

	adapter := newMockAdapter("test-adapter")
	remoteTask := RemoteTask{
		RemoteID: "remote-1",
		Title:    "Task",
		Status:   "needsAction",
		ETag:     "e1",
	}
	adapter.setInitialTasks([]RemoteTask{remoteTask})

	engine.RegisterAdapter(adapter)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	// Manually trigger
	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	// Verify sync happened (task imported)
	tasks, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 task after manual trigger, got %d", len(tasks))
	}
}

// TestSyncEngine_BugFix_SilentPushFailureNoSyncMapEntry verifies that when
// the adapter's Push silently drops a Create (no PushResult emitted —
// equivalent to SPC returning success=false), the engine does not record a
// sync_map entry for that task. Historically the engine iterated `changes`
// instead of `results`, so failed creates produced phantom sync_map rows
// with locally-generated remote_ids that SPC never echoed back, and which
// the hard-delete detector then refused to clean up.
func TestSyncEngine_BugFix_SilentPushFailureNoSyncMapEntry(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	adapter := newMockAdapter("test-adapter")
	adapter.failCreate = true
	engine.RegisterAdapter(adapter)

	ctx := context.Background()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	localTask := &taskstore.Task{
		Title:        taskstore.SqlStr("Phantom"),
		Status:       taskstore.SqlStr("needsAction"),
		IsDeleted:    "N",
		IsReminderOn: "N",
	}
	if err := store.Create(ctx, localTask); err != nil {
		t.Fatalf("Create: %v", err)
	}

	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	syncMap := NewSyncMap(db)
	entry, err := syncMap.GetByTaskID(ctx, localTask.TaskID, "test-adapter")
	if err != nil {
		t.Fatalf("GetByTaskID: %v", err)
	}
	if entry != nil {
		t.Errorf("expected no sync_map entry after silent push failure, got %+v", entry)
	}
}

// TestSyncEngine_BugFix_LocalEditNotMaskedByETagMatchPull verifies that a
// local edit between syncs is still pushed even after a no-op (ETag-match)
// pull. Previously the engine bumped LastPulled to "now" on every observed
// pull, which advanced the findLocalChanges cutoff past the local
// modification timestamp and silently dropped the edit forever.
func TestSyncEngine_BugFix_LocalEditNotMaskedByETagMatchPull(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	adapter := newMockAdapter("test-adapter")
	initial := RemoteTask{
		RemoteID: "remote-task-1",
		Title:    "Initial",
		Status:   "needsAction",
		ETag:     "etag-1",
	}
	adapter.setInitialTasks([]RemoteTask{initial})
	engine.RegisterAdapter(adapter)

	ctx := context.Background()
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	// First sync: import the remote task into local store.
	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	tasks, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task after import, got %d", len(tasks))
	}
	imported := tasks[0]

	// Wait a tick so localModified after the edit is strictly later than
	// the import's LastPulled — otherwise the millisecond-resolution clock
	// can blur the ordering.
	time.Sleep(5 * time.Millisecond)

	// User edits the task locally.
	imported.Status = taskstore.SqlStr("completed")
	if err := store.Update(ctx, &imported); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Second sync: remote unchanged (same ETag). Engine should still
	// detect the local edit and push it.
	adapter.pushes = nil
	beforeTs = engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	if len(adapter.pushes) != 1 || len(adapter.pushes[0]) != 1 {
		t.Fatalf("expected 1 push with 1 change after local edit, got pushes=%v", adapter.pushes)
	}
	change := adapter.pushes[0][0]
	if change.Type != ChangeUpdate {
		t.Errorf("expected ChangeUpdate, got %v", change.Type)
	}
	if change.Remote.Status != "completed" {
		t.Errorf("expected pushed status=completed, got %q", change.Remote.Status)
	}
}

// TestSyncEngine_BugFix_PhantomEntryNotHardDeleted verifies that an entry
// pushed but never observed in a pull (LastSeen == 0) is NOT
// hard-delete-detected. This protects freshly-pushed tasks from being
// soft-deleted before the remote has propagated and echoed them back.
// Pre-existing phantoms (from the silent-failure era) also stay in place
// rather than thrashing through a re-create loop.
func TestSyncEngine_BugFix_PhantomEntryNotHardDeleted(t *testing.T) {
	db := openTestDB(t)
	store := taskdb.NewStore(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	engine := NewSyncEngine(store, db, logger, 1*time.Hour)

	ctx := context.Background()

	localTask := &taskstore.Task{
		Title:        taskstore.SqlStr("Just-pushed task"),
		Status:       taskstore.SqlStr("needsAction"),
		IsDeleted:    "N",
		IsReminderOn: "N",
	}
	if err := store.Create(ctx, localTask); err != nil {
		t.Fatalf("Create: %v", err)
	}

	syncMap := NewSyncMap(db)
	if err := syncMap.Upsert(ctx, &SyncMapEntry{
		TaskID:     localTask.TaskID,
		AdapterID:  "test-adapter",
		RemoteID:   "r-orphan-1",
		LastPushed: time.Now().UnixMilli(),
		LastPulled: 0,
		LastSeen:   0,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Adapter has no remote tasks — pull returns empty.
	adapter := newMockAdapter("test-adapter")
	engine.RegisterAdapter(adapter)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer engine.Stop()

	beforeTs := engine.Status().LastSyncAt
	engine.TriggerSync()
	waitForSync(t, engine, beforeTs)

	got, err := store.Get(ctx, localTask.TaskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.IsDeleted == "Y" {
		t.Error("phantom entry (LastSeen=0) should not have been soft-deleted")
	}

	entry, err := syncMap.GetByTaskID(ctx, localTask.TaskID, "test-adapter")
	if err != nil {
		t.Fatalf("GetByTaskID: %v", err)
	}
	if entry == nil {
		t.Error("phantom sync_map entry should be preserved (not auto-cleaned)")
	}
}
