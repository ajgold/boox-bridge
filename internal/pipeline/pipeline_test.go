package pipeline

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/notestore"
	"github.com/sysop/ultrabridge/internal/processor"
)

func openTestComponents(t *testing.T) (*notestore.Store, *processor.Store, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return notestore.New(db, dir), processor.New(db, processor.WorkerConfig{}), dir
}

// openTestComponentsWithDB returns the shared *sql.DB in addition to Store and Processor,
// so tests can manipulate job/notes state via raw SQL without accessing unexported fields.
func openTestComponentsWithDB(t *testing.T) (*notestore.Store, *processor.Store, *sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := notedb.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("notedb.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return notestore.New(db, dir), processor.New(db, processor.WorkerConfig{}), db, dir
}

// AC8.3 + AC8.1: Reconciler queues new files, not unchanged files
func TestReconciler_NewAndUnchanged(t *testing.T) {
	ns, proc, dir := openTestComponents(t)
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: nil})

	notePath := filepath.Join(dir, "test.note")
	os.WriteFile(notePath, []byte("data"), 0644)

	// AC8.1: first reconcile discovers and queues the new file
	pl.reconcile(context.Background())
	if proc.Status().Pending != 1 {
		t.Errorf("pending after first scan = %d, want 1", proc.Status().Pending)
	}

	// AC8.3: second reconcile (same mtime) does not re-queue
	pl.reconcile(context.Background())
	if proc.Status().Pending != 1 {
		t.Errorf("pending after second scan = %d, want 1 (unchanged file should not re-queue)", proc.Status().Pending)
	}
}

// AC8.2: Changed file (mtime bump) is re-queued
func TestReconciler_ChangedFileRequeued(t *testing.T) {
	ns, proc, dir := openTestComponents(t)
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: nil})

	notePath := filepath.Join(dir, "test.note")
	os.WriteFile(notePath, []byte("v1"), 0644)
	pl.reconcile(context.Background())

	// Bump mtime
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(notePath, future, future)
	pl.reconcile(context.Background())

	// Should now have a second pending entry (or re-queued)
	if proc.Status().Pending < 1 {
		t.Error("expected re-queued job after mtime bump")
	}
}

// AC8.4: Debounce coalesces rapid writes into a single enqueue
func TestWatcher_Debounce(t *testing.T) {
	ns, proc, dir := openTestComponents(t)
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: nil})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pl.Start(ctx)
	defer pl.Close()

	notePath := filepath.Join(dir, "rapid.note")
	for i := 0; i < 5; i++ {
		os.WriteFile(notePath, []byte("data"), 0644)
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for debounce to expire
	time.Sleep(debounceDelay + time.Second)

	st := proc.Status()
	if st.Pending != 1 {
		t.Errorf("pending = %d after 5 rapid writes, want 1 (debounce should coalesce)", st.Pending)
	}
}

// TestPipeline_MoveDetection_JobTransferred verifies AC1.1 and AC1.3:
// when a file with the same content appears at a new path, the reconciler
// transfers the done job to the new path without re-enqueueing.
func TestPipeline_MoveDetection_JobTransferred(t *testing.T) {
	ns, proc, db, dir := openTestComponentsWithDB(t)
	ctx := context.Background()

	// Create a file at pathA.
	pathA := filepath.Join(dir, "original.note")
	content := []byte("supernote note content for hash test")
	if err := os.WriteFile(pathA, content, 0644); err != nil {
		t.Fatalf("write pathA: %v", err)
	}

	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})

	// First reconcile: discovers pathA, creates pending job.
	pl.reconcile(ctx)

	// Simulate completed processing: set job to done and store hash.
	hashA, err := notestore.ComputeSHA256(pathA)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	if err := ns.SetHash(ctx, pathA, hashA); err != nil {
		t.Fatalf("SetHash: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE jobs SET status='done', finished_at=? WHERE note_path=?",
		time.Now().Unix(), pathA); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	// Create pathB with identical content (simulates a move/rename).
	pathB := filepath.Join(dir, "moved.note")
	if err := os.WriteFile(pathB, content, 0644); err != nil {
		t.Fatalf("write pathB: %v", err)
	}

	// Second reconcile: discovers pathB (new mtime), should detect hash match and transfer.
	pl.reconcile(ctx)

	// AC1.1: pathB should have a done job (transferred from pathA).
	jobB, err := proc.GetJob(ctx, pathB)
	if err != nil {
		t.Fatalf("GetJob(pathB): %v", err)
	}
	if jobB == nil {
		t.Fatal("expected job for pathB after transfer, got nil")
	}
	if jobB.Status != processor.StatusDone {
		t.Errorf("pathB job status = %q, want done", jobB.Status)
	}

	// AC1.1: pathA should have no job (transferred away).
	jobA, _ := proc.GetJob(ctx, pathA)
	if jobA != nil {
		t.Errorf("expected no job for pathA after transfer, got status=%q", jobA.Status)
	}

	// AC1.3: pathB's notes.sha256 should be set.
	var sha256B string
	db.QueryRowContext(ctx, "SELECT COALESCE(sha256,'') FROM notes WHERE path=?", pathB).Scan(&sha256B)
	if sha256B == "" {
		t.Error("notes.sha256 for pathB should be set after transfer")
	}
	if sha256B != hashA {
		t.Errorf("notes.sha256 = %q, want %q", sha256B, hashA)
	}
}

// TestEnqueue_SkipsConflictFiles verifies that files containing _CONFLICT_ in
// the name are not enqueued. The device creates these when both local and cloud
// versions of a file changed since last sync; processing them causes feedback loops.
func TestEnqueue_SkipsConflictFiles(t *testing.T) {
	ns, proc, dir := openTestComponents(t)
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})

	// Create a _CONFLICT_ file
	conflictPath := filepath.Join(dir, "myfile_CONFLICT_20260322.note")
	os.WriteFile(conflictPath, []byte("data"), 0644)

	pl.enqueue(context.Background(), conflictPath)

	if proc.Status().Pending != 0 {
		t.Errorf("pending = %d, want 0 (_CONFLICT_ files should be skipped)", proc.Status().Pending)
	}

	// Verify a normal file IS still enqueued
	normalPath := filepath.Join(dir, "normal.note")
	os.WriteFile(normalPath, []byte("data"), 0644)
	pl.enqueue(context.Background(), normalPath)
	if proc.Status().Pending != 1 {
		t.Errorf("pending = %d, want 1 (normal files should be enqueued)", proc.Status().Pending)
	}
}

// TestEnqueue_PublicWrapper_FKSafe: the exported Enqueue upserts the notes row
// before inserting the job, so a fresh (never-seen) file enqueues without the
// jobs.note_path → notes(path) FK violation that a raw processor.Enqueue hits.
// Reproduces the Phase 4d hardware bug where the upload OCR-kick called
// processor.Enqueue directly and failed with "FOREIGN KEY constraint failed".
func TestEnqueue_PublicWrapper_FKSafe(t *testing.T) {
	ns, proc, dir := openTestComponents(t)
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})

	p := filepath.Join(dir, "uploaded.note")
	if err := os.WriteFile(p, []byte("device upload bytes"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := pl.Enqueue(context.Background(), p); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if proc.Status().Pending != 1 {
		t.Fatalf("pending = %d, want 1 (Enqueue must upsert then queue)", proc.Status().Pending)
	}
}

// TestPipeline_MoveDetection_ContentChanged verifies AC1.4:
// when the moved file has different content, it gets enqueued normally.
func TestPipeline_MoveDetection_ContentChanged(t *testing.T) {
	ns, proc, db, dir := openTestComponentsWithDB(t)
	ctx := context.Background()

	// Create pathA, simulate done job with hash.
	pathA := filepath.Join(dir, "original.note")
	if err := os.WriteFile(pathA, []byte("original content"), 0644); err != nil {
		t.Fatalf("write pathA: %v", err)
	}
	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})
	pl.reconcile(ctx)

	hashA, _ := notestore.ComputeSHA256(pathA)
	ns.SetHash(ctx, pathA, hashA)
	db.ExecContext(ctx, "UPDATE jobs SET status='done' WHERE note_path=?", pathA)

	// Create pathB with DIFFERENT content.
	pathB := filepath.Join(dir, "different.note")
	if err := os.WriteFile(pathB, []byte("completely different content"), 0644); err != nil {
		t.Fatalf("write pathB: %v", err)
	}

	pl.reconcile(ctx)

	// AC1.4: pathB should be enqueued (not transferred) because content differs.
	jobB, err := proc.GetJob(ctx, pathB)
	if err != nil {
		t.Fatalf("GetJob(pathB): %v", err)
	}
	if jobB == nil {
		t.Fatal("expected pathB to be enqueued, got nil job")
	}
	if jobB.Status != processor.StatusPending {
		t.Errorf("pathB job status = %q, want pending", jobB.Status)
	}
}

// TestEnqueue_HashChange_FileChanged verifies note-reprocessing.AC3.1:
// When a done job's file content has changed (hash differs), the file is re-queued as pending with requeue delay.
func TestEnqueue_HashChange_FileChanged(t *testing.T) {
	ns, proc, db, dir := openTestComponentsWithDB(t)
	ctx := context.Background()

	// Create a file with initial content.
	path := filepath.Join(dir, "test.note")
	v1Content := []byte("version 1 content")
	if err := os.WriteFile(path, v1Content, 0644); err != nil {
		t.Fatalf("write v1: %v", err)
	}

	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})

	// First reconcile: discovers the file, creates pending job.
	pl.reconcile(ctx)

	// Simulate completed processing: mark job as done, store hash.
	hash1, err := notestore.ComputeSHA256(path)
	if err != nil {
		t.Fatalf("ComputeSHA256 v1: %v", err)
	}
	if err := ns.SetHash(ctx, path, hash1); err != nil {
		t.Fatalf("SetHash: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE jobs SET status='done', finished_at=? WHERE note_path=?",
		time.Now().Unix(), path); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	// Verify job is done.
	job, err := proc.GetJob(ctx, path)
	if err != nil {
		t.Fatalf("GetJob after done: %v", err)
	}
	if job.Status != processor.StatusDone {
		t.Errorf("job status = %q, want done", job.Status)
	}

	// Now modify file content (simulating user edit on device).
	v2Content := []byte("version 2 content - user edited")
	if err := os.WriteFile(path, v2Content, 0644); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	// Call enqueue again — should detect hash mismatch and re-queue with delay.
	pl.enqueue(ctx, path)

	// AC3.1: Job should now be pending with requeue_after set.
	job, err = proc.GetJob(ctx, path)
	if err != nil {
		t.Fatalf("GetJob after re-enqueue: %v", err)
	}
	if job == nil {
		t.Fatal("expected job after re-enqueue, got nil")
	}
	if job.Status != processor.StatusPending {
		t.Errorf("job status = %q, want pending after file change", job.Status)
	}
	if job.RequeueAfter == nil {
		t.Error("expected requeue_after to be set, got nil")
	}
}

// TestEnqueue_HashChange_FileUnchanged verifies note-reprocessing.AC3.2:
// When a done job's file content hasn't changed (hash matches), the file is skipped.
func TestEnqueue_HashChange_FileUnchanged(t *testing.T) {
	ns, proc, db, dir := openTestComponentsWithDB(t)
	ctx := context.Background()

	// Create a file.
	path := filepath.Join(dir, "test.note")
	content := []byte("file content that won't change")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})

	// First reconcile: discovers the file, creates pending job.
	pl.reconcile(ctx)

	// Simulate completed processing: mark job as done, store hash.
	hash, err := notestore.ComputeSHA256(path)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	if err := ns.SetHash(ctx, path, hash); err != nil {
		t.Fatalf("SetHash: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE jobs SET status='done', finished_at=? WHERE note_path=?",
		time.Now().Unix(), path); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	// Verify job is done.
	job, err := proc.GetJob(ctx, path)
	if err != nil {
		t.Fatalf("GetJob after done: %v", err)
	}
	if job.Status != processor.StatusDone {
		t.Errorf("job status = %q, want done", job.Status)
	}

	// Call enqueue again WITHOUT changing file — should skip.
	pl.enqueue(ctx, path)

	// AC3.2: Job should still be done, not re-queued.
	job, err = proc.GetJob(ctx, path)
	if err != nil {
		t.Fatalf("GetJob after enqueue: %v", err)
	}
	if job == nil {
		t.Fatal("expected job, got nil")
	}
	if job.Status != processor.StatusDone {
		t.Errorf("job status = %q, want done (no re-queue)", job.Status)
	}
}

// TestEnqueue_HashChange_NoStoredHash verifies note-reprocessing.AC3.3:
// When a done job has no stored hash (NULL sha256), the file is re-queued (conservative behavior).
func TestEnqueue_HashChange_NoStoredHash(t *testing.T) {
	ns, proc, db, dir := openTestComponentsWithDB(t)
	ctx := context.Background()

	// Create a file.
	path := filepath.Join(dir, "test.note")
	content := []byte("file with no stored hash")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})

	// First reconcile: discovers the file, creates pending job.
	pl.reconcile(ctx)

	// Mark job as done but DO NOT store hash (simulating legacy file).
	if _, err := db.ExecContext(ctx,
		"UPDATE jobs SET status='done', finished_at=? WHERE note_path=?",
		time.Now().Unix(), path); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	// Verify job is done and hash is NULL.
	var status string
	var storedHash sql.NullString
	if err := db.QueryRowContext(ctx,
		"SELECT status, COALESCE(sha256, NULL) FROM jobs j JOIN notes n ON n.path=j.note_path WHERE j.note_path=?",
		path).Scan(&status, &storedHash); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if status != processor.StatusDone {
		t.Errorf("job status = %q, want done", status)
	}
	if storedHash.Valid {
		t.Error("expected NULL hash, got value")
	}

	// Call enqueue — should detect NULL hash and re-queue (conservative).
	pl.enqueue(ctx, path)

	// AC3.3: Job should now be pending (re-queued due to NULL hash).
	job, err := proc.GetJob(ctx, path)
	if err != nil {
		t.Fatalf("GetJob after enqueue: %v", err)
	}
	if job == nil {
		t.Fatal("expected job, got nil")
	}
	if job.Status != processor.StatusPending {
		t.Errorf("job status = %q, want pending (re-queued due to NULL hash)", job.Status)
	}
}

// TestEnqueue_RapidEdits verifies note-reprocessing.AC3.4:
// Rapid successive file edits within the requeue delay window result in only one pending job.
func TestEnqueue_RapidEdits(t *testing.T) {
	ns, proc, db, dir := openTestComponentsWithDB(t)
	ctx := context.Background()

	// Create a file.
	path := filepath.Join(dir, "test.note")
	content := []byte("initial content")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	pl := New(Config{NotesPath: dir, Store: ns, Proc: proc, Logger: slog.Default()})

	// First reconcile: discovers the file, creates pending job.
	pl.reconcile(ctx)

	// Simulate completed processing: mark job as done, store hash.
	hash1, err := notestore.ComputeSHA256(path)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	if err := ns.SetHash(ctx, path, hash1); err != nil {
		t.Fatalf("SetHash: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE jobs SET status='done', finished_at=? WHERE note_path=?",
		time.Now().Unix(), path); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	// Modify file.
	if err := os.WriteFile(path, []byte("edit 1"), 0644); err != nil {
		t.Fatalf("write edit 1: %v", err)
	}

	// First enqueue: detects change, re-queues to pending.
	pl.enqueue(ctx, path)

	// Verify job is now pending.
	job1, err := proc.GetJob(ctx, path)
	if err != nil {
		t.Fatalf("GetJob after first enqueue: %v", err)
	}
	if job1 == nil {
		t.Fatal("expected job after first enqueue, got nil")
	}
	if job1.Status != processor.StatusPending {
		t.Errorf("job status after first enqueue = %q, want pending", job1.Status)
	}
	initialRequeueAfter := job1.RequeueAfter

	// Modify file again (rapid edit within delay window).
	if err := os.WriteFile(path, []byte("edit 2"), 0644); err != nil {
		t.Fatalf("write edit 2: %v", err)
	}

	// Second enqueue: ON CONFLICT clause only affects done/failed/skipped jobs.
	// Since the job is now pending, the second enqueue is a no-op (no row match).
	pl.enqueue(ctx, path)

	// AC3.4: Job should still be pending with the SAME requeue_after from first enqueue.
	job2, err := proc.GetJob(ctx, path)
	if err != nil {
		t.Fatalf("GetJob after second enqueue: %v", err)
	}
	if job2 == nil {
		t.Fatal("expected job after second enqueue, got nil")
	}
	if job2.Status != processor.StatusPending {
		t.Errorf("job status after second enqueue = %q, want pending", job2.Status)
	}
	if (job2.RequeueAfter == nil) != (initialRequeueAfter == nil) ||
		(job2.RequeueAfter != nil && initialRequeueAfter != nil && !job2.RequeueAfter.Equal(*initialRequeueAfter)) {
		var t2, t1 string
		if job2.RequeueAfter == nil {
			t2 = "nil"
		} else {
			t2 = job2.RequeueAfter.String()
		}
		if initialRequeueAfter == nil {
			t1 = "nil"
		} else {
			t1 = initialRequeueAfter.String()
		}
		t.Errorf("requeue_after changed from %s to %s (should be unchanged)", t1, t2)
	}
}
