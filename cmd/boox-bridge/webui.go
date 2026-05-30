package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed webui.html
var webuiHTML []byte

// webServer exposes a small HTML+JSON UI for queue management at
// boox.jacomail.com (NPM → :3000). No auth in the daemon — relies on NPM
// access-list or tailnet-only reachability.
type webServer struct {
	cfg   *config
	dedup *dedup
	spend *spend
}

func newWebServer(cfg *config, d *dedup, s *spend) *webServer {
	return &webServer{cfg: cfg, dedup: d, spend: s}
}

func (w *webServer) listen(ctx context.Context) error {
	if w.cfg.WebListenAddr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", w.handleHome)
	mux.HandleFunc("GET /api/state", w.handleState)
	mux.HandleFunc("POST /api/retry", w.handleRetry)
	mux.HandleFunc("POST /api/skip", w.handleSkip)
	mux.HandleFunc("POST /api/delete", w.handleDelete)

	srv := &http.Server{
		Addr:              w.cfg.WebListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	slog.Info("web ui listening", "addr", w.cfg.WebListenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (w *webServer) handleHome(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = rw.Write(webuiHTML)
}

type webEntry struct {
	Name          string `json:"name"`
	Path          string `json:"path,omitempty"`
	Size          int64  `json:"size"`
	MTime         string `json:"mtime,omitempty"`
	Attempt       int    `json:"attempt,omitempty"`
	Max           int    `json:"max,omitempty"`
	NextAttemptAt string `json:"next_attempt_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

type stateResp struct {
	Inbox         []webEntry `json:"inbox"`
	Retry         []webEntry `json:"retry"`
	DLQ           []webEntry `json:"dlq"`
	SpendTodayUSD float64    `json:"spend_today_usd"`
	MaxDailyUSD   float64    `json:"max_daily_usd"`
	SeenCount     int        `json:"seen_count"`
}

func (w *webServer) handleState(rw http.ResponseWriter, r *http.Request) {
	st := stateResp{
		MaxDailyUSD:   w.cfg.MaxDailyUSD,
		SpendTodayUSD: w.spend.cap - w.spend.remaining(),
		SeenCount:     w.dedup.size(),
	}
	st.Inbox = w.listInbox()
	st.Retry = w.listRetry()
	st.DLQ = w.listDLQ()
	writeJSON(rw, http.StatusOK, st)
}

func (w *webServer) listInbox() []webEntry {
	var out []webEntry
	_ = filepath.WalkDir(w.cfg.InboxDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".note") {
			return nil
		}
		info, _ := d.Info()
		out = append(out, webEntry{
			Name:  d.Name(),
			Path:  p,
			Size:  info.Size(),
			MTime: info.ModTime().UTC().Format(time.RFC3339),
		})
		return nil
	})
	sortByName(out)
	return out
}

func (w *webServer) listRetry() []webEntry {
	var out []webEntry
	entries, err := os.ReadDir(w.cfg.RetryDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".retry.json") {
			continue
		}
		sidecarPath := filepath.Join(w.cfg.RetryDir(), e.Name())
		data, err := os.ReadFile(sidecarPath)
		if err != nil {
			continue
		}
		var rec retryRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		notePath := filepath.Join(w.cfg.RetryDir(), rec.BaseName)
		info, statErr := os.Stat(notePath)
		entry := webEntry{
			Name:          rec.BaseName,
			Path:          notePath,
			Attempt:       rec.AttemptCount,
			Max:           w.cfg.MaxRetries,
			NextAttemptAt: rec.NextAttemptAt.UTC().Format(time.RFC3339),
			LastError:     rec.LastError,
		}
		if statErr == nil {
			entry.Size = info.Size()
		}
		out = append(out, entry)
	}
	sortByName(out)
	return out
}

func (w *webServer) listDLQ() []webEntry {
	var out []webEntry
	entries, err := os.ReadDir(w.cfg.DLQDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".err") {
			continue
		}
		info, _ := e.Info()
		errPath := filepath.Join(w.cfg.DLQDir(), e.Name()+".err")
		errMsg := ""
		if data, err := os.ReadFile(errPath); err == nil {
			errMsg = strings.TrimSpace(string(data))
		}
		out = append(out, webEntry{
			Name:      e.Name(),
			Path:      filepath.Join(w.cfg.DLQDir(), e.Name()),
			Size:      info.Size(),
			MTime:     info.ModTime().UTC().Format(time.RFC3339),
			LastError: errMsg,
		})
	}
	sortByName(out)
	return out
}

func sortByName(es []webEntry) {
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
}

// resolveFile locates the file in the named queue. Whitelists the queue
// name so the caller can't path-escape via ../ tricks.
func (w *webServer) resolveFile(queue, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid file name")
	}
	switch queue {
	case "inbox":
		// Inbox is recursive; walk to find by basename.
		var hit string
		_ = filepath.WalkDir(w.cfg.InboxDir(), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if d.Name() == name {
				hit = p
				return filepath.SkipAll
			}
			return nil
		})
		if hit == "" {
			return "", fmt.Errorf("file not found in inbox: %s", name)
		}
		return hit, nil
	case "retry":
		p := filepath.Join(w.cfg.RetryDir(), name)
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("file not found in retry: %s", name)
		}
		return p, nil
	case "dlq":
		p := filepath.Join(w.cfg.DLQDir(), name)
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("file not found in dlq: %s", name)
		}
		return p, nil
	}
	return "", fmt.Errorf("unknown queue: %s", queue)
}

func (w *webServer) handleRetry(rw http.ResponseWriter, r *http.Request) {
	queue := r.URL.Query().Get("queue")
	name := r.URL.Query().Get("file")
	src, err := w.resolveFile(queue, name)
	if err != nil {
		writeErr(rw, http.StatusBadRequest, err)
		return
	}
	dst := filepath.Join(w.cfg.InboxDir(), name)
	if err := os.MkdirAll(w.cfg.InboxDir(), 0o750); err != nil {
		writeErr(rw, http.StatusInternalServerError, err)
		return
	}
	if err := os.Rename(src, dst); err != nil {
		writeErr(rw, http.StatusInternalServerError, err)
		return
	}
	// Clean up retry sidecar / dlq .err companion if present.
	if queue == "retry" {
		_ = os.Remove(src + ".retry.json")
		_ = os.Remove(filepath.Join(w.cfg.RetryDir(), name+".retry.json"))
	}
	if queue == "dlq" {
		_ = os.Remove(src + ".err")
	}
	slog.Info("web_retry", "queue", queue, "file", name)
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok": true, "message": "moved to inbox: " + name,
	})
}

func (w *webServer) handleSkip(rw http.ResponseWriter, r *http.Request) {
	queue := r.URL.Query().Get("queue")
	name := r.URL.Query().Get("file")
	src, err := w.resolveFile(queue, name)
	if err != nil {
		writeErr(rw, http.StatusBadRequest, err)
		return
	}
	// Hash so it never reprocesses.
	hash, err := hashFile(src)
	if err != nil {
		writeErr(rw, http.StatusInternalServerError, err)
		return
	}
	if err := w.dedup.mark(hash); err != nil {
		writeErr(rw, http.StatusInternalServerError, err)
		return
	}
	// Move to archive.
	day := time.Now().In(w.spend.tz).Format("2006-01-02")
	dstDir := filepath.Join(w.cfg.ArchiveDir(), day)
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		writeErr(rw, http.StatusInternalServerError, err)
		return
	}
	if err := os.Rename(src, filepath.Join(dstDir, name)); err != nil {
		writeErr(rw, http.StatusInternalServerError, err)
		return
	}
	if queue == "retry" {
		_ = os.Remove(filepath.Join(w.cfg.RetryDir(), name+".retry.json"))
	}
	if queue == "dlq" {
		_ = os.Remove(filepath.Join(w.cfg.DLQDir(), name+".err"))
	}
	slog.Info("web_skip", "queue", queue, "file", name, "hash", hash[:12])
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok": true, "message": "skipped + archived: " + name,
	})
}

func (w *webServer) handleDelete(rw http.ResponseWriter, r *http.Request) {
	queue := r.URL.Query().Get("queue")
	name := r.URL.Query().Get("file")
	src, err := w.resolveFile(queue, name)
	if err != nil {
		writeErr(rw, http.StatusBadRequest, err)
		return
	}
	if err := os.Remove(src); err != nil {
		writeErr(rw, http.StatusInternalServerError, err)
		return
	}
	if queue == "retry" {
		_ = os.Remove(filepath.Join(w.cfg.RetryDir(), name+".retry.json"))
	}
	if queue == "dlq" {
		_ = os.Remove(filepath.Join(w.cfg.DLQDir(), name+".err"))
	}
	slog.Info("web_delete", "queue", queue, "file", name)
	writeJSON(rw, http.StatusOK, map[string]any{
		"ok": true, "message": "deleted: " + name,
	})
}

func writeJSON(rw http.ResponseWriter, status int, payload any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(payload)
}

func writeErr(rw http.ResponseWriter, status int, err error) {
	writeJSON(rw, status, map[string]any{"ok": false, "error": err.Error()})
}
