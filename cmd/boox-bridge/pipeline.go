package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sysop/ultrabridge/internal/booxnote"
	"github.com/sysop/ultrabridge/internal/booxrender"
)

type pipeline struct {
	cfg    *config
	dedup  *dedup
	spend  *spend
	hwr    *hwrClient
	affine *affineClient
	spool  *spool
}

// process runs the full ingest pipeline on a single .note file. Each
// stage is logged on its own line via slog so failures unambiguously
// identify the failing stage.
func (p *pipeline) process(ctx context.Context, path string) {
	t0 := time.Now()
	name := filepath.Base(path)
	log := slog.With("file", name)

	// 1. dedup
	hash, err := hashFile(path)
	if err != nil {
		p.fail(log, path, "hash", err, t0)
		return
	}
	log = log.With("hash", hash[:12])
	if p.dedup.has(hash) {
		log.Info("dedup_skip", "stage", "dedup")
		if err := p.move(path, p.cfg.ArchiveDir()); err != nil {
			log.Warn("archive_after_dedup_failed", "err", err)
		}
		return
	}

	// 2. spend cap
	if rem := p.spend.remaining(); rem <= 0 {
		log.Warn("spend_cap_hit", "stage", "spend", "remaining_usd", rem)
		p.toDLQ(path, fmt.Errorf("daily LLM spend cap reached"))
		return
	}

	// 3. parse
	tParse := time.Now()
	note, err := openNote(path)
	if err != nil {
		p.fail(log, path, "parse", err, t0)
		return
	}
	log.Info("parsed", "stage", "parse", "dur_ms", time.Since(tParse).Milliseconds(),
		"title", note.Title, "pages", len(note.Pages))
	if len(note.Pages) > p.cfg.MaxPagesPerNote {
		p.fail(log, path, "page_cap",
			fmt.Errorf("note has %d pages, cap is %d", len(note.Pages), p.cfg.MaxPagesPerNote), t0)
		return
	}

	// 4. render
	tRender := time.Now()
	pageImgs := make([][]byte, 0, len(note.Pages))
	for i, page := range note.Pages {
		img, err := booxrender.RenderPage(page)
		if err != nil {
			p.fail(log, path, "render", fmt.Errorf("page %d: %w", i+1, err), t0)
			return
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			p.fail(log, path, "encode", fmt.Errorf("page %d: %w", i+1, err), t0)
			return
		}
		pageImgs = append(pageImgs, buf.Bytes())
	}
	log.Info("rendered", "stage", "render", "dur_ms", time.Since(tRender).Milliseconds(),
		"pages", len(pageImgs))

	// 5. HWR (two-pass: Sonnet → escalate to Opus on low confidence)
	tHWR := time.Now()
	hwrRes, err := p.hwr.transcribe(ctx, pageImgs)
	if err != nil {
		// Rate-limit failures aren't real failures — defer via the spool.
		var rle *rateLimitErr
		if errors.As(err, &rle) {
			attempt, schedErr := p.spool.schedule(path, rle.RetryAfter, err.Error())
			if schedErr == nil {
				log.Info("retry_scheduled", "stage", "hwr", "attempt", attempt,
					"delay_s", int(rle.RetryAfter.Seconds()),
					"upstream", rle.Message)
				return
			}
			if errors.Is(schedErr, errRetryExhausted) {
				p.fail(log, path, "hwr_retries_exhausted", err, t0)
				return
			}
			log.Warn("spool_schedule_failed", "err", schedErr)
		}
		p.fail(log, path, "hwr", err, t0)
		return
	}
	// Merge Boox device tags (read directly from the .note ZIP's tag/pb/*
	// protobuf) with any tags the HWR model spotted on the page. Boox tags
	// are the canonical signal — Claire's intent.
	deviceTags := extractBooxTags(path)
	hwrRes.Tags = mergeTags(deviceTags, hwrRes.Tags)
	log.Info("hwr_done", "stage", "hwr", "dur_ms", time.Since(tHWR).Milliseconds(),
		"model", hwrRes.Model, "confidence", hwrRes.Confidence,
		"illegible", hwrRes.IllegibleCount, "cost_usd", hwrRes.CostUSD,
		"tags", hwrRes.Tags, "device_tags", deviceTags)

	// 6. Affine ingest
	tPub := time.Now()
	title := hwrRes.Title
	if title == "" {
		title = note.Title
	}
	if title == "" {
		title = "Boox note " + time.Now().In(p.spend.tz).Format("2006-01-02 15:04")
	}
	// Flag for human review only when the HWR model marked enough words
	// illegible to suggest the page is genuinely hard to read. Self-reported
	// confidence ("medium" vs "high") is too noisy on doctor cursive — Sonnet
	// returns "medium" on virtually every page even when the transcription
	// is fine. Per-word [?marker] count is the cleaner signal.
	if hwrRes.IllegibleCount > p.cfg.NeedsReviewIllegibleThreshold {
		title = "[needs review] " + title
	}
	docID, err := p.affine.publish(ctx, publishReq{
		Title:        title,
		BodyMarkdown: hwrRes.BodyMarkdown,
		Tags:         hwrRes.Tags,
		PagePNGs:     pageImgs,
	})
	if err != nil {
		p.fail(log, path, "publish", err, t0)
		return
	}
	log.Info("published", "stage", "publish", "dur_ms", time.Since(tPub).Milliseconds(),
		"doc_id", docID)

	// 7. mark seen + archive
	if err := p.dedup.mark(hash); err != nil {
		log.Warn("dedup_mark_failed", "err", err)
	}
	if err := p.move(path, p.cfg.ArchiveDir()); err != nil {
		log.Warn("archive_failed", "err", err)
	}
	log.Info("done", "total_ms", time.Since(t0).Milliseconds())
}

func openNote(path string) (*booxnote.Note, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return booxnote.Open(f, st.Size())
}

// fail logs an error and moves the file to the DLQ, but doesn't crash
// the daemon — pipeline keeps running for subsequent files.
func (p *pipeline) fail(log *slog.Logger, path, stage string, err error, t0 time.Time) {
	log.Error(stage+"_failed", "stage", stage, "dur_ms", time.Since(t0).Milliseconds(), "err", err)
	p.toDLQ(path, err)
}

func (p *pipeline) toDLQ(path string, cause error) {
	if err := os.MkdirAll(p.cfg.DLQDir(), 0o750); err != nil {
		slog.Error("dlq_mkdir_failed", "err", err)
		return
	}
	dst := filepath.Join(p.cfg.DLQDir(), filepath.Base(path))
	if err := os.Rename(path, dst); err != nil {
		slog.Error("dlq_move_failed", "src", path, "dst", dst, "err", err)
		return
	}
	errPath := dst + ".err"
	_ = os.WriteFile(errPath, []byte(cause.Error()+"\n"), 0o640)
}

func (p *pipeline) move(src, dstRoot string) error {
	day := time.Now().In(p.spend.tz).Format("2006-01-02")
	dstDir := filepath.Join(dstRoot, day)
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return err
	}
	dst := filepath.Join(dstDir, filepath.Base(src))
	if err := os.Rename(src, dst); err != nil {
		// Cross-device or other rename failure — fall back to copy+remove.
		if !errors.Is(err, os.ErrNotExist) {
			if copyErr := copyAndRemove(src, dst); copyErr != nil {
				return fmt.Errorf("move: rename=%v copy=%w", err, copyErr)
			}
			return nil
		}
		return err
	}
	return nil
}

func copyAndRemove(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(df, sf); err != nil {
		df.Close()
		return err
	}
	if err := df.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

// pngBase64 is a small helper used by hwr.go when packing images into
// the Claude Messages payload. Kept here to share buffer reuse.
func pngBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// mergeTags returns the union of a and b in stable order (a first), case-
// preserving but case-insensitive for dedup so "adam" and "Adam" collapse.
func mergeTags(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	for _, s := range a {
		add(s)
	}
	for _, s := range b {
		add(s)
	}
	return out
}
