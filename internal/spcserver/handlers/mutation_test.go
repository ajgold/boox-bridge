package handlers

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
)

func newMutationHandler(t *testing.T, root string) (*MutationHandler, *fileids.Registry) {
	t.Helper()
	ctx := context.Background()
	db, err := notedb.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := fileids.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	reg := fileids.New(db, root)
	return &MutationHandler{Root: root, Reg: reg}, reg
}

// AC4.1: delete_folder_v3 soft-deletes — the file moves under .recycle/, leaves
// its original path, and the VO reports its metadata.
func TestDeleteFolderSoftDeletes(t *testing.T) {
	root := t.TempDir()
	noteDir := filepath.Join(root, "Note")
	if err := os.MkdirAll(noteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(noteDir, "doomed.note")
	if err := os.WriteFile(abs, []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, reg := newMutationHandler(t, root)
	id, err := reg.IDFor(context.Background(), abs)
	if err != nil {
		t.Fatal(err)
	}

	out := decodeMap(t, h.DeleteFolder, `{"equipmentNo":"SN078","id":"`+strconv.FormatInt(id, 10)+`"}`)
	if out["success"] != true {
		t.Fatalf("success = %v (%v)", out["success"], out)
	}
	meta, _ := out["metadata"].(map[string]any)
	if meta == nil || meta["name"] != "doomed.note" || meta["path_display"] != "/Note/doomed.note" {
		t.Fatalf("metadata = %v", meta)
	}

	// Original path is gone.
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("original path should be gone, err=%v", err)
	}
	// A copy now lives somewhere under .recycle/.
	recycleRoot := filepath.Join(root, ".recycle")
	var found bool
	_ = filepath.Walk(recycleRoot, func(p string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() && info.Name() == "doomed.note" {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatalf("deleted file not found under .recycle/")
	}
}

// AC4.1: an unknown id → success:false with E0318, never a 500.
func TestDeleteFolderUnknownID(t *testing.T) {
	root := t.TempDir()
	h, _ := newMutationHandler(t, root)
	out := decodeMap(t, h.DeleteFolder, `{"equipmentNo":"SN078","id":"999999"}`)
	if out["success"] != false {
		t.Fatalf("unknown id should fail softly, got %v", out)
	}
	if out["errorCode"] != errDeleteMissingCode {
		t.Fatalf("errorCode = %v, want %s", out["errorCode"], errDeleteMissingCode)
	}
}
