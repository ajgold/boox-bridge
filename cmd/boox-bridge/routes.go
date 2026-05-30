package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
)

// routeTarget is one resolution endpoint — workspace and (optionally) the
// parent doc to nest the new doc under.
type routeTarget struct {
	WorkspaceID string `json:"workspace_id"`
	ParentDocID string `json:"parent_doc_id"`
}

// routeMapping pins a Boox tag (case-insensitive) to a routing target.
type routeMapping struct {
	Tag string `json:"tag"`
	routeTarget
}

// routesConfig is the persisted form on disk.
type routesConfig struct {
	Default         routeTarget       `json:"default"`
	Mappings        []routeMapping    `json:"mappings"`
	WorkspaceLabels map[string]string `json:"workspace_labels,omitempty"` // user-defined friendly names for the workspace dropdowns
}

// routes is the in-memory + on-disk router. All access serialises through
// `mu`; readers (Resolve) take RLock, writers (Set / Reload) take Lock.
type routes struct {
	path string

	mu  sync.RWMutex
	cfg routesConfig
}

// uuidish matches both classic UUIDs and the shorter random-ID strings
// Affine sometimes uses for its docs (e.g. "S5GQRh5nis"). We use it
// defensively to catch obviously-empty or accidentally-pasted control
// characters; we do NOT enforce strict UUID-v4 shape, because Affine's
// own docIds aren't UUIDs.
var uuidish = regexp.MustCompile(`^[A-Za-z0-9_-]{6,64}$`)

// errMissingBootstrap signals that both routes.json and the env fallback
// are absent — daemon should exit non-zero with a clear message.
var errMissingBootstrap = errors.New(
	"no routes.json and no AFFINE_WORKSPACE_ID env fallback — set the env var for first start, or provision /var/lib/boox/state/routes.json",
)

// loadRoutes opens or initialises the routes file. If the file doesn't
// exist and `fallbackDefault.WorkspaceID` is non-empty, a fresh file is
// written with that default and an empty mappings slice. If both the file
// and the fallback are missing, returns errMissingBootstrap.
func loadRoutes(path string, fallbackDefault routeTarget) (*routes, error) {
	r := &routes{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read routes.json: %w", err)
		}
		if strings.TrimSpace(fallbackDefault.WorkspaceID) == "" {
			return nil, errMissingBootstrap
		}
		r.cfg = routesConfig{Default: fallbackDefault, Mappings: []routeMapping{}}
		if err := r.writeLocked(); err != nil {
			return nil, fmt.Errorf("bootstrap routes.json: %w", err)
		}
		return r, nil
	}
	var cfg routesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse routes.json: %w", err)
	}
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid routes.json: %w", err)
	}
	r.cfg = cfg
	return r, nil
}

// Resolve picks the target for a note. Iterates mappings outer / tags
// inner so the mapping list's order is the *sole* priority signal,
// regardless of the order tags appear on the note.
func (r *routes) Resolve(tags []string) (workspaceID, parentDocID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.cfg.Mappings) > 0 {
		lower := make([]string, 0, len(tags))
		for _, t := range tags {
			t = strings.TrimSpace(strings.ToLower(t))
			if t != "" {
				lower = append(lower, t)
			}
		}
		for _, m := range r.cfg.Mappings {
			needle := strings.ToLower(strings.TrimSpace(m.Tag))
			for _, have := range lower {
				if have == needle {
					return m.WorkspaceID, m.ParentDocID
				}
			}
		}
	}
	return r.cfg.Default.WorkspaceID, r.cfg.Default.ParentDocID
}

// ResolveVia is like Resolve but also returns the tag that won, or "" if
// the default was used. Useful for logging.
func (r *routes) ResolveVia(tags []string) (workspaceID, parentDocID, viaTag string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.cfg.Mappings) > 0 {
		lower := make([]string, 0, len(tags))
		for _, t := range tags {
			t = strings.TrimSpace(strings.ToLower(t))
			if t != "" {
				lower = append(lower, t)
			}
		}
		for _, m := range r.cfg.Mappings {
			needle := strings.ToLower(strings.TrimSpace(m.Tag))
			for _, have := range lower {
				if have == needle {
					return m.WorkspaceID, m.ParentDocID, m.Tag
				}
			}
		}
	}
	return r.cfg.Default.WorkspaceID, r.cfg.Default.ParentDocID, ""
}

// Get returns a snapshot of the current config.
func (r *routes) Get() routesConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := routesConfig{Default: r.cfg.Default}
	out.Mappings = append([]routeMapping(nil), r.cfg.Mappings...)
	if len(r.cfg.WorkspaceLabels) > 0 {
		out.WorkspaceLabels = make(map[string]string, len(r.cfg.WorkspaceLabels))
		for k, v := range r.cfg.WorkspaceLabels {
			out.WorkspaceLabels[k] = v
		}
	}
	return out
}

// Set validates, writes atomically, and swaps the in-memory copy — all
// inside one critical section so Get can never read a half-applied state.
func (r *routes) Set(cfg routesConfig) error {
	if err := validate(cfg); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.cfg
	r.cfg = cfg
	if err := r.writeLocked(); err != nil {
		r.cfg = prev
		return err
	}
	return nil
}

// Reload re-reads the file from disk. Bound to SIGHUP for hand-edits.
func (r *routes) Reload() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	var cfg routesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if err := validate(cfg); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	return nil
}

// writeLocked persists `r.cfg` to disk. Caller must hold the write lock.
func (r *routes) writeLocked() error {
	data, err := json.MarshalIndent(r.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// validate checks the structural invariants the rest of the daemon relies on.
func validate(cfg routesConfig) error {
	if !uuidish.MatchString(strings.TrimSpace(cfg.Default.WorkspaceID)) {
		return fmt.Errorf("default.workspace_id must be non-empty")
	}
	seen := make(map[string]struct{}, len(cfg.Mappings))
	for i, m := range cfg.Mappings {
		tag := strings.TrimSpace(m.Tag)
		if tag == "" {
			return fmt.Errorf("mappings[%d].tag is empty", i)
		}
		key := strings.ToLower(tag)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("mappings[%d].tag %q is a duplicate (case-insensitive)", i, tag)
		}
		seen[key] = struct{}{}
		if !uuidish.MatchString(strings.TrimSpace(m.WorkspaceID)) {
			return fmt.Errorf("mappings[%d].workspace_id is empty or malformed", i)
		}
		if m.ParentDocID != "" && !uuidish.MatchString(strings.TrimSpace(m.ParentDocID)) {
			return fmt.Errorf("mappings[%d].parent_doc_id is malformed", i)
		}
	}
	if cfg.Default.ParentDocID != "" && !uuidish.MatchString(strings.TrimSpace(cfg.Default.ParentDocID)) {
		return fmt.Errorf("default.parent_doc_id is malformed")
	}
	return nil
}
