package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watch tails the inbox directory and calls fn(path) for each completed
// .note file. A file is "completed" when:
//   - filename matches *.note (not .tmp, .part, dotfile)
//   - no fsnotify event for DebounceSeconds
//   - file size hasn't changed for 3 s
type processFn func(ctx context.Context, path string)

func watch(ctx context.Context, cfg *config, fn processFn) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	if err := os.MkdirAll(cfg.InboxDir(), 0o750); err != nil {
		return err
	}
	// fsnotify doesn't recurse. Walk and add a watcher to every existing
	// subdirectory; new dirs are added on the fly when a Create event fires.
	if err := addTreeWatchers(w, cfg.InboxDir()); err != nil {
		return err
	}
	slog.Info("watching", "dir", cfg.InboxDir(), "debounce_s", cfg.DebounceSeconds, "recursive", true)

	// Pending timer per path.
	type pending struct {
		timer *time.Timer
	}
	mu := sync.Mutex{}
	timers := make(map[string]*pending)

	schedule := func(path string) {
		mu.Lock()
		defer mu.Unlock()
		if p, ok := timers[path]; ok {
			p.timer.Reset(time.Duration(cfg.DebounceSeconds) * time.Second)
			return
		}
		t := time.AfterFunc(time.Duration(cfg.DebounceSeconds)*time.Second, func() {
			mu.Lock()
			delete(timers, path)
			mu.Unlock()

			if !isStable(path) {
				slog.Warn("size_unstable_skip", "path", path)
				return
			}
			// Run synchronously — one file at a time keeps the pipeline
			// simple and matches Claire's expected volume.
			fn(ctx, path)
		})
		timers[path] = &pending{timer: t}
	}

	// Sweep at startup so files that landed while the daemon was down
	// still get processed. Walk the whole tree — Boox creates per-device
	// subdirectories like onyx/NoteAir4C/Notebooks/.
	_ = filepath.WalkDir(cfg.InboxDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if shouldProcess(d.Name()) {
			schedule(p)
		}
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("watcher events channel closed")
			}
			// Boox creates per-device subdirs on first sync — attach a
			// watcher to any new directory so files inside it are seen.
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
					if err := addTreeWatchers(w, ev.Name); err != nil {
						slog.Warn("add_subdir_watcher_failed", "dir", ev.Name, "err", err)
					}
					continue
				}
			}
			if !shouldProcess(filepath.Base(ev.Name)) {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 {
				schedule(ev.Name)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return errors.New("watcher errors channel closed")
			}
			slog.Warn("fsnotify_err", "err", err)
		}
	}
}

// addTreeWatchers walks root and adds an fsnotify watcher to every
// directory it finds. Idempotent — fsnotify silently dedupes duplicate
// Add() calls.
func addTreeWatchers(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if addErr := w.Add(p); addErr != nil {
				slog.Warn("watcher_add_failed", "dir", p, "err", addErr)
			}
		}
		return nil
	})
}

func shouldProcess(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".part") {
		return false
	}
	return strings.HasSuffix(lower, ".note")
}

// isStable reports whether the file's size is unchanged across a 3-second
// window — guards against torn reads of partially-uploaded WebDAV files.
func isStable(path string) bool {
	first, err := os.Stat(path)
	if err != nil {
		return false
	}
	time.Sleep(3 * time.Second)
	second, err := os.Stat(path)
	if err != nil {
		return false
	}
	return first.Size() == second.Size() && first.Size() > 0
}
