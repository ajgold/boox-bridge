package supernote

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sysop/ultrabridge/internal/taskdb"
	"github.com/sysop/ultrabridge/internal/taskstore"
	"github.com/sysop/ultrabridge/internal/tasksync"
)

// testMigrationDB creates an in-memory SQLite DB with the necessary schema for migration tests.
func testMigrationDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}

	// Create tasks table
	_, err = db.Exec(`
		CREATE TABLE tasks (
			task_id TEXT PRIMARY KEY,
			title TEXT,
			detail TEXT,
			status TEXT,
			importance TEXT,
			due_time INTEGER DEFAULT 0,
			completed_time INTEGER,
			last_modified INTEGER,
			recurrence TEXT,
			is_reminder_on TEXT DEFAULT 'N',
			links TEXT,
			is_deleted TEXT DEFAULT 'N',
			ical_blob TEXT,
			created_at INTEGER,
			updated_at INTEGER
		)
	`)
	if err != nil {
		t.Fatalf("create tasks table: %v", err)
	}

	// Create sync_state table
	_, err = db.Exec(`
		CREATE TABLE sync_state (
			adapter_id TEXT PRIMARY KEY,
			last_sync_token TEXT,
			last_sync_at INTEGER
		)
	`)
	if err != nil {
		t.Fatalf("create sync_state table: %v", err)
	}

	// Create task_sync_map table
	_, err = db.Exec(`
		CREATE TABLE task_sync_map (
			task_id TEXT NOT NULL,
			adapter_id TEXT NOT NULL,
			remote_id TEXT NOT NULL,
			remote_etag TEXT,
			last_pushed_at INTEGER,
			last_pulled_at INTEGER,
			last_seen_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (task_id, adapter_id)
		)
	`)
	if err != nil {
		t.Fatalf("create task_sync_map table: %v", err)
	}

	return db
}

// mockMigrationSPCServer mocks the SPC REST API for migration testing.
type mockMigrationSPCServer struct {
	server  *httptest.Server
	mu      sync.Mutex
	token   string
	tasks   map[string]SPCTask
	loginFails bool
}

func newMockMigrationSPCServer(initialTasks []SPCTask) *mockMigrationSPCServer {
	m := &mockMigrationSPCServer{
		token: "test-jwt-token-" + fmt.Sprint(time.Now().UnixNano()),
		tasks: make(map[string]SPCTask),
	}

	// Populate initial tasks
	for _, t := range initialTasks {
		m.tasks[t.ID] = t
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()

		path := r.RequestURI
		if idx := strings.Index(path, "?"); idx >= 0 {
			path = path[:idx]
		}

		switch path {
		case "/api/official/user/query/random/code":
			resp := map[string]interface{}{
				"success":    true,
				"randomCode": "test-challenge-" + fmt.Sprint(time.Now().UnixNano()),
				"timestamp":  time.Now().UnixMilli(),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		case "/api/official/user/account/login/new":
			if m.loginFails {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]interface{}{"success": false})
				return
			}
			resp := map[string]interface{}{
				"success": true,
				"token":   m.token,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		case "/api/file/schedule/task/all":
			if r.Header.Get("x-access-token") != m.token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var tasksList []SPCTask
			for _, t := range m.tasks {
				tasksList = append(tasksList, t)
			}
			resp := map[string]interface{}{
				"success":      true,
				"scheduleTask": tasksList,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return m
}

func (m *mockMigrationSPCServer) close() {
	m.server.Close()
}

// TestMigration_AC4_1_ImportsTasks verifies that MigrateFromSPC imports active tasks
// and creates sync map entries, skipping deleted tasks.
func TestMigration_AC4_1_ImportsTasks(t *testing.T) {
	// Setup: 5 tasks (3 active, 2 deleted)
	initialTasks := []SPCTask{
		{
			ID:            "spc-task-1",
			Title:         "Active Task 1",
			Detail:        "Task 1 detail",
			Status:        "needsAction",
			Importance:    "high",
			DueTime:       1609459200000,
			CompletedTime: 1609372800000,
			LastModified:  1609372800000,
			Recurrence:    "DAILY",
			IsReminderOn:  "Y",
			Links:         "http://example.com",
			IsDeleted:     "N",
		},
		{
			ID:            "spc-task-2",
			Title:         "Active Task 2",
			Detail:        "Task 2 detail",
			Status:        "completed",
			Importance:    "medium",
			DueTime:       1609545600000,
			CompletedTime: 1609459200000,
			LastModified:  1609459200000,
			IsReminderOn:  "N",
			IsDeleted:     "N",
		},
		{
			ID:            "spc-task-3",
			Title:         "Active Task 3",
			Status:        "needsAction",
			IsDeleted:     "N",
		},
		{
			ID:            "spc-deleted-1",
			Title:         "Deleted Task 1",
			Status:        "needsAction",
			IsDeleted:     "Y",
		},
		{
			ID:            "spc-deleted-2",
			Title:         "Deleted Task 2",
			Status:        "needsAction",
			IsDeleted:     "Y",
		},
	}

	mock := newMockMigrationSPCServer(initialTasks)
	defer mock.close()

	db := testMigrationDB(t)
	defer db.Close()

	// Create clients
	client := NewClient(mock.server.URL, "test@example.com", "testpass", testLogger())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("client login failed: %v", err)
	}

	// Create a mock store
	store := &mockTaskStore{
		tasks: make(map[string]*taskstore.Task),
	}

	// Create sync map
	syncMap := tasksync.NewSyncMap(db)

	// Execute migration
	imported, err := MigrateFromSPC(context.Background(), client, store, syncMap, testLogger())
	if err != nil {
		t.Fatalf("MigrateFromSPC failed: %v", err)
	}

	// Verify 3 tasks imported
	if imported != 3 {
		t.Errorf("expected 3 tasks imported, got %d", imported)
	}

	// Verify tasks in store
	if len(store.tasks) != 3 {
		t.Errorf("expected 3 tasks in store, got %d", len(store.tasks))
	}

	// Verify specific tasks are present
	if _, exists := store.tasks["spc-task-1"]; !exists {
		t.Errorf("task spc-task-1 not found in store")
	}
	if _, exists := store.tasks["spc-task-2"]; !exists {
		t.Errorf("task spc-task-2 not found in store")
	}
	if _, exists := store.tasks["spc-task-3"]; !exists {
		t.Errorf("task spc-task-3 not found in store")
	}

	// Verify deleted tasks not present
	if _, exists := store.tasks["spc-deleted-1"]; exists {
		t.Errorf("deleted task spc-deleted-1 should not be in store")
	}
	if _, exists := store.tasks["spc-deleted-2"]; exists {
		t.Errorf("deleted task spc-deleted-2 should not be in store")
	}

	// Verify field mapping for first task
	t1 := store.tasks["spc-task-1"]
	if !t1.Title.Valid || t1.Title.String != "Active Task 1" {
		t.Errorf("task title mismatch: got %v", t1.Title)
	}
	if !t1.Detail.Valid || t1.Detail.String != "Task 1 detail" {
		t.Errorf("task detail mismatch: got %v", t1.Detail)
	}
	if !t1.Status.Valid || t1.Status.String != "needsAction" {
		t.Errorf("task status mismatch: got %v", t1.Status)
	}
	if !t1.Importance.Valid || t1.Importance.String != "high" {
		t.Errorf("task importance mismatch: got %v", t1.Importance)
	}
	if t1.DueTime != 1609459200000 {
		t.Errorf("task due_time mismatch: got %d", t1.DueTime)
	}
	// Verify CompletedTime quirk is preserved
	if !t1.CompletedTime.Valid || t1.CompletedTime.Int64 != 1609372800000 {
		t.Errorf("task completed_time mismatch: got %v", t1.CompletedTime)
	}

	// Verify sync map entries created
	entry1, err := syncMap.GetByTaskID(context.Background(), "spc-task-1", "supernote")
	if err != nil {
		t.Fatalf("failed to get sync map entry: %v", err)
	}
	if entry1 == nil {
		t.Errorf("sync map entry for spc-task-1 not found")
	} else {
		if entry1.RemoteID != "spc-task-1" {
			t.Errorf("sync map remote_id mismatch: got %s", entry1.RemoteID)
		}
		if entry1.AdapterID != "supernote" {
			t.Errorf("sync map adapter_id mismatch: got %s", entry1.AdapterID)
		}
		if entry1.RemoteETag == "" {
			t.Errorf("sync map remote_etag should not be empty")
		}
		if entry1.LastPushed == 0 {
			t.Errorf("sync map last_pushed should be set")
		}
		if entry1.LastPulled == 0 {
			t.Errorf("sync map last_pulled should be set")
		}
	}

	// Verify all 3 active tasks have sync map entries
	for _, taskID := range []string{"spc-task-1", "spc-task-2", "spc-task-3"} {
		entry, err := syncMap.GetByTaskID(context.Background(), taskID, "supernote")
		if err != nil {
			t.Fatalf("failed to get sync map entry for %s: %v", taskID, err)
		}
		if entry == nil {
			t.Errorf("sync map entry for %s not found", taskID)
		}
	}

	// Verify deleted tasks don't have sync map entries
	entry, err := syncMap.GetByTaskID(context.Background(), "spc-deleted-1", "supernote")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entry != nil {
		t.Errorf("deleted task spc-deleted-1 should not have sync map entry")
	}
}

// TestMigration_AC4_2_IsEmpty verifies IsEmpty behavior before and after migration.
// Uses the real taskdb.Store to test the actual IsEmpty method and Create path.
func TestMigration_AC4_2_IsEmpty(t *testing.T) {
	db := testMigrationDB(t)
	defer db.Close()

	mock := newMockMigrationSPCServer([]SPCTask{
		{
			ID:        "spc-task-1",
			Title:     "Task 1",
			Status:    "needsAction",
			IsDeleted: "N",
		},
	})
	defer mock.close()

	client := NewClient(mock.server.URL, "test@example.com", "testpass", testLogger())
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("client login failed: %v", err)
	}

	// Use real taskdb.Store so IsEmpty and Create exercise production code
	store := taskdb.NewStore(db)
	syncMap := tasksync.NewSyncMap(db)

	// Test 1: Fresh DB should be empty
	isEmpty, err := store.IsEmpty(context.Background())
	if err != nil {
		t.Fatalf("IsEmpty on fresh DB failed: %v", err)
	}
	if !isEmpty {
		t.Errorf("expected fresh DB to be empty, got false")
	}

	// Run migration using real store
	_, err = MigrateFromSPC(context.Background(), client, store, syncMap, testLogger())
	if err != nil {
		t.Fatalf("MigrateFromSPC failed: %v", err)
	}

	// Test 2: After import, DB should not be empty
	isEmpty, err = store.IsEmpty(context.Background())
	if err != nil {
		t.Fatalf("IsEmpty after migration failed: %v", err)
	}
	if isEmpty {
		t.Errorf("expected DB after migration to not be empty, got true")
	}
}

// TestMigration_AC4_3_LoginFailure verifies error handling when SPC login fails.
func TestMigration_AC4_3_LoginFailure(t *testing.T) {
	db := testMigrationDB(t)
	defer db.Close()

	mock := newMockMigrationSPCServer([]SPCTask{
		{
			ID:        "spc-task-1",
			Title:     "Task 1",
			Status:    "needsAction",
			IsDeleted: "N",
		},
	})
	defer mock.close()

	// Create client with login failure
	mock.mu.Lock()
	mock.loginFails = true
	mock.mu.Unlock()

	client := NewClient(mock.server.URL, "test@example.com", "testpass", testLogger())

	// Login should fail
	err := client.Login(context.Background())
	if err == nil {
		t.Errorf("expected login to fail, but it succeeded")
	}

	// Create store
	store := &mockTaskStore{
		tasks: make(map[string]*taskstore.Task),
	}

	syncMap := tasksync.NewSyncMap(db)

	// Try migration with failed client (it will try to FetchTasks anyway)
	// The error should be returned and no tasks imported
	_, err = MigrateFromSPC(context.Background(), client, store, syncMap, testLogger())
	if err == nil {
		t.Errorf("expected MigrateFromSPC to return error after login failure, got nil")
	}

	// Verify store is still empty
	if len(store.tasks) != 0 {
		t.Errorf("expected store to be empty, got %d tasks", len(store.tasks))
	}

	// Verify DB is still empty (no partial import)
	realStore := taskdb.NewStore(db)
	isEmpty, isEmptyErr := realStore.IsEmpty(context.Background())
	if isEmptyErr != nil {
		t.Fatalf("IsEmpty check failed: %v", isEmptyErr)
	}
	if !isEmpty {
		t.Errorf("expected DB to be empty after migration failure")
	}
}

// mockTaskStore implements TaskCreator for testing.
type mockTaskStore struct {
	mu    sync.Mutex
	tasks map[string]*taskstore.Task
}

func (m *mockTaskStore) Create(ctx context.Context, t *taskstore.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Store a copy
	copy := *t
	m.tasks[t.TaskID] = &copy
	return nil
}

