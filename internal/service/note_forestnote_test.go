package service

import (
	"context"
	"encoding/binary"
	"io"
	"testing"

	"github.com/sysop/ultrabridge/internal/syncstore"
)

// fakeFNReader is a canned ForestNoteReader for testing the note service's
// ForestNote surfacing without a real syncstore.
type fakeFNReader struct {
	folders   []syncstore.FolderRow
	notebooks []syncstore.NotebookRow
	pages     map[string][]syncstore.PageRef
	strokes   map[string][]syncstore.StrokeData
	meta      map[string]syncstore.NotebookRow
}

func (f *fakeFNReader) ListFolders(context.Context) ([]syncstore.FolderRow, error) {
	return f.folders, nil
}
func (f *fakeFNReader) ListNotebooks(context.Context) ([]syncstore.NotebookRow, error) {
	return f.notebooks, nil
}
func (f *fakeFNReader) NotebookPages(_ context.Context, nb string) ([]syncstore.PageRef, error) {
	return f.pages[nb], nil
}
func (f *fakeFNReader) NotebookMeta(_ context.Context, nb string) (syncstore.NotebookRow, error) {
	return f.meta[nb], nil
}
func (f *fakeFNReader) LivePageStrokes(_ context.Context, pg string) ([]syncstore.StrokeData, error) {
	return f.strokes[pg], nil
}

func twoPointStroke() syncstore.StrokeData {
	buf := make([]byte, 0, 40)
	for _, v := range []int32{10, 10, 1000, 0, 0, 80, 120, 1000, 0, 5} {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(v))
		buf = append(buf, b[:]...)
	}
	return syncstore.StrokeData{Color: -16777216, PenWidthMin: 2, PenWidthMax: 6, Points: buf, Z: 0}
}

func TestRenderForestNotePage_ReturnsJPEG(t *testing.T) {
	r := &fakeFNReader{strokes: map[string][]syncstore.StrokeData{
		"00000000000000000000000PGA": {twoPointStroke()},
	}}
	s := &noteService{fnReader: r}
	rc, ct, err := s.RenderPage(context.Background(), "forestnote://00000000000000000000000NBA/00000000000000000000000PGA", 0)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	defer rc.Close()
	if ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
	body, _ := io.ReadAll(rc)
	if len(body) == 0 {
		t.Error("empty image body")
	}
}

func TestRenderForestNotePage_NilReader(t *testing.T) {
	s := &noteService{}
	if _, _, err := s.RenderPage(context.Background(), "forestnote://nb/pg", 0); err == nil {
		t.Error("want error when forestnote reader not wired, got nil")
	}
}

func TestListForestNotePages_BuildsPaths(t *testing.T) {
	r := &fakeFNReader{
		meta:  map[string]syncstore.NotebookRow{"nbA": {ID: "nbA", Name: "Journal"}},
		pages: map[string][]syncstore.PageRef{"nbA": {{ID: "pgA"}, {ID: "pgB"}}},
	}
	s := &noteService{fnReader: r}
	name, pages, err := s.ListForestNotePages(context.Background(), "nbA")
	if err != nil {
		t.Fatalf("list pages: %v", err)
	}
	if name != "Journal" {
		t.Errorf("name = %q, want Journal", name)
	}
	if len(pages) != 2 || pages[0].Path != "forestnote://nbA/pgA" || pages[1].Ordinal != 1 {
		t.Errorf("pages = %+v", pages)
	}
}

func TestBuildForestNoteTree_NestingAndUnfiled(t *testing.T) {
	folders := []syncstore.FolderRow{
		{ID: "f1", Name: "Parent"},
		{ID: "f2", Name: "Child", ParentFolderID: "f1"},
	}
	notebooks := []syncstore.NotebookRow{
		{ID: "n1", Name: "InChild", FolderID: "f2", PageCount: 3},
		{ID: "n2", Name: "Loose"},                    // unfiled (no folder)
		{ID: "n3", Name: "Orphan", FolderID: "gone"}, // folder missing → unfiled
	}
	roots, unfiled := buildForestNoteTree(folders, notebooks)
	if len(roots) != 1 || roots[0].FolderID != "f1" {
		t.Fatalf("roots = %+v, want single f1 root", roots)
	}
	if len(roots[0].Children) != 1 || roots[0].Children[0].FolderID != "f2" {
		t.Fatalf("f1 children = %+v, want [f2]", roots[0].Children)
	}
	if len(roots[0].Children[0].Notebooks) != 1 || roots[0].Children[0].Notebooks[0].NotebookID != "n1" {
		t.Errorf("f2 notebooks = %+v, want [n1]", roots[0].Children[0].Notebooks)
	}
	if len(unfiled) != 2 {
		t.Errorf("unfiled = %+v, want 2 (loose + orphan)", unfiled)
	}
}
