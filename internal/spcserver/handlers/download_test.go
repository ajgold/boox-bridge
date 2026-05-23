package handlers

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sysop/ultrabridge/internal/notedb"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/oss"
)

const testOssSecret = "test-oss-secret"

// newDownloadHandler builds a DownloadHandler over a temp root with a fresh
// in-memory registry and a fixed-secret signer. Returns the handler and the
// registry (so tests can mint ids via IDFor).
func newDownloadHandler(t *testing.T, root string) (*DownloadHandler, *fileids.Registry) {
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
	return &DownloadHandler{
		Root:   root,
		Reg:    reg,
		Signer: &oss.Signer{Secret: testOssSecret},
	}, reg
}

func md5Hex(t *testing.T, b []byte) string {
	t.Helper()
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// Covers: spc-phase-3.AC2.1, spc-phase-3.AC2.3
func TestDownloadV3Success(t *testing.T) {
	root := t.TempDir()
	noteDir := filepath.Join(root, "Note")
	if err := os.MkdirAll(noteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("hello supernote")
	abs := filepath.Join(noteDir, "foo.note")
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		t.Fatal(err)
	}
	h, reg := newDownloadHandler(t, root)
	id, err := reg.IDFor(context.Background(), abs)
	if err != nil {
		t.Fatal(err)
	}

	out := decodeMap(t, h.DownloadV3, `{"equipmentNo":"SN078","id":`+strconv.FormatInt(id, 10)+`}`)

	if out["success"] != true {
		t.Fatalf("success = %v; want true (body=%v)", out["success"], out)
	}
	if out["name"] != "foo.note" {
		t.Errorf("name = %v; want foo.note", out["name"])
	}
	if out["path_display"] != "/Note/foo.note" {
		t.Errorf("path_display = %v; want /Note/foo.note", out["path_display"])
	}
	if out["content_hash"] != md5Hex(t, content) {
		t.Errorf("content_hash = %v; want %s", out["content_hash"], md5Hex(t, content))
	}
	if out["is_downloadable"] != true {
		t.Errorf("is_downloadable = %v; want true", out["is_downloadable"])
	}
	if out["size"] != float64(len(content)) {
		t.Errorf("size = %v; want %d", out["size"], len(content))
	}
	if out["id"] != strconv.FormatInt(id, 10) {
		t.Errorf("id = %v; want %d (String)", out["id"], id)
	}

	// AC2.3: the url is a valid /api/oss/download URL whose signature validates
	// and whose encoded path decrypts to path_display.
	rawURL, _ := out["url"].(string)
	u, err := url.Parse(rawURL)
	if err != nil || u.Path != "/api/oss/download" {
		t.Fatalf("url = %q; parse err %v, path %q", rawURL, err, u.Path)
	}
	q := u.Query()
	ts, err := strconv.ParseInt(q.Get("timestamp"), 10, 64)
	if err != nil {
		t.Fatalf("bad timestamp %q: %v", q.Get("timestamp"), err)
	}
	verifier := &oss.Signer{Secret: testOssSecret}
	if !verifier.ValidateDownload(q.Get("signature"), ts, q.Get("nonce"), q.Get("path")) {
		t.Errorf("minted signature does not validate: %s", rawURL)
	}
	decoded, err := oss.DecryptPath(q.Get("path"))
	if err != nil || decoded != "/Note/foo.note" {
		t.Errorf("decoded path = %q (err %v); want /Note/foo.note", decoded, err)
	}
}

// Covers: spc-phase-3.AC2.2
func TestDownloadV3UnknownID(t *testing.T) {
	h, _ := newDownloadHandler(t, t.TempDir())
	out := decodeMap(t, h.DownloadV3, `{"equipmentNo":"SN078","id":99999}`)
	if out["success"] != false {
		t.Errorf("success = %v; want false for unknown id", out["success"])
	}
	if out["errorCode"] != "E0321" {
		t.Errorf("errorCode = %v; want E0321", out["errorCode"])
	}
}

// Covers: spc-phase-3.AC2.2 (registered id whose file was removed)
func TestDownloadV3DeletedFile(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "gone.note")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, reg := newDownloadHandler(t, root)
	id, err := reg.IDFor(context.Background(), abs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	out := decodeMap(t, h.DownloadV3, `{"equipmentNo":"SN078","id":`+strconv.FormatInt(id, 10)+`}`)
	if out["success"] != false || out["errorCode"] != "E0321" {
		t.Errorf("deleted file: success=%v errorCode=%v; want false/E0321", out["success"], out["errorCode"])
	}
}

// Covers: spc-phase-3.AC2.2 (null id)
func TestDownloadV3NullID(t *testing.T) {
	h, _ := newDownloadHandler(t, t.TempDir())
	out := decodeMap(t, h.DownloadV3, `{"equipmentNo":"SN078"}`)
	if out["success"] != false || out["errorCode"] != "E0321" {
		t.Errorf("null id: success=%v errorCode=%v; want false/E0321", out["success"], out["errorCode"])
	}
}

// Covers: spc-phase-3.AC2.4
func TestGenerateDownloadURL(t *testing.T) {
	h, _ := newDownloadHandler(t, t.TempDir())
	req := httptest.NewRequest(http.MethodPost,
		"/?filePath=/Note&fileName=foo.note&pathId=42", nil)
	rec := httptest.NewRecorder()
	h.GenerateDownloadURL(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	// FileDownloadApplyVO is NOT a BaseVO — there is no "success" key.
	if _, hasSuccess := out["success"]; hasSuccess {
		t.Errorf("FileDownloadApplyVO should not carry a success field: %v", out)
	}
	if out["pathId"] != "42" {
		t.Errorf("pathId = %v; want 42", out["pathId"])
	}
	rawURL, _ := out["url"].(string)
	u, err := url.Parse(rawURL)
	if err != nil || u.Path != "/api/oss/download" {
		t.Fatalf("url = %q; parse err %v", rawURL, err)
	}
	q := u.Query()
	ts, _ := strconv.ParseInt(q.Get("timestamp"), 10, 64)
	verifier := &oss.Signer{Secret: testOssSecret}
	if !verifier.ValidateDownload(q.Get("signature"), ts, q.Get("nonce"), q.Get("path")) {
		t.Errorf("generated signature does not validate: %s", rawURL)
	}
	if out["signature"] != q.Get("signature") {
		t.Errorf("VO signature %v != url signature %v", out["signature"], q.Get("signature"))
	}
}
