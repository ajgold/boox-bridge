package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/spcserver/dto"
	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
	"github.com/sysop/ultrabridge/internal/spcserver/fileids"
	"github.com/sysop/ultrabridge/internal/spcserver/mapping"
)

// recycleDir is the dot-prefixed soft-delete area under FILE_ROOT. Like
// .staging it is excluded from list_folder (isHidden), so recycled files vanish
// from the device's tree without being destroyed. Recycle-bin CRUD (list/
// restore/purge) is Phase 5; Phase 4 only moves files in.
const recycleDir = ".recycle"

// errDeleteMissing is FileErrorCodeEnum.E0318 ("The folder or file you want to
// delete does not exist"), returned by delete_folder_v3 for an unknown/stale id.
const (
	errDeleteMissingCode = "E0318"
	errDeleteMissingMsg  = "The folder or file you want to delete does not exist"
)

// MutationHandler serves the SPC file-mutation write-path (Phase 4c): delete
// (soft, to .recycle/), move, and copy. It shares the FileHandler's Root and
// registry. Notifier (optional) fires a best-effort FILE-SYN after a change.
type MutationHandler struct {
	Root     string
	Reg      *fileids.Registry
	Notifier UploadNotifier
	Now      func() time.Time
	Logger   *slog.Logger
}

func (h *MutationHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

func (h *MutationHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// DeleteFolder soft-deletes a file/folder: it moves the target under
// <FILE_ROOT>/.recycle/<timestamp>/<originalRelPath> (preserving the original
// layout for a future restore) and reports the deleted entry's metadata.
// POST /api/file/3/files/delete_folder_v3 (F_FileLocalController.java:123).
func (h *MutationHandler) DeleteFolder(w http.ResponseWriter, r *http.Request) {
	var req dto.DeleteFolderLocalDTO
	_ = json.NewDecoder(r.Body).Decode(&req)

	fail := func() {
		envelope.WriteJSON(w, dto.DeleteFolderLocalVO{
			BaseVO:      envelope.BaseVO{Success: false, ErrorCode: errDeleteMissingCode, ErrorMsg: errDeleteMissingMsg},
			EquipmentNo: req.EquipmentNo,
		})
	}

	id, perr := strconv.ParseInt(req.ID, 10, 64)
	if perr != nil || h.Root == "" {
		fail()
		return
	}
	abs, found, err := h.Reg.PathFor(r.Context(), id)
	if err != nil || !found {
		fail()
		return
	}
	if _, err := os.Lstat(abs); err != nil {
		fail() // registered id whose file is already gone
		return
	}

	// Capture metadata before the move (path_display/name/id from the live entry).
	entry, err := mapping.EntryFor(r.Context(), h.Root, abs, h.Reg)
	if err != nil {
		h.log().Error("delete_folder_v3 EntryFor", "path", abs, "err", err)
		fail()
		return
	}

	if err := h.recycle(entry.PathDisplay, abs); err != nil {
		h.log().Error("delete_folder_v3 recycle", "path", abs, "err", err)
		fail()
		return
	}

	if h.Notifier != nil {
		_ = h.Notifier.NotifyFile(r.Context())
	}
	envelope.WriteJSON(w, dto.DeleteFolderLocalVO{
		BaseVO:      envelope.OK(),
		EquipmentNo: req.EquipmentNo,
		Metadata: &dto.MetadataVO{
			Tag:         entry.Tag,
			ID:          entry.ID,
			Name:        entry.Name,
			PathDisplay: entry.PathDisplay,
		},
	})
}

// recycle moves abs (whose root-relative path is pathDisplay) under
// .recycle/<millis>/<relPath>, creating parents. The timestamped generation
// keeps repeated deletes of the same path from colliding.
func (h *MutationHandler) recycle(pathDisplay, abs string) error {
	rel := strings.TrimPrefix(pathDisplay, "/")
	gen := strconv.FormatInt(h.now().UnixMilli(), 10)
	dest := filepath.Join(h.Root, recycleDir, gen, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.Rename(abs, dest)
}
