package forestpdf

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// jpegBytes makes a small solid-color JPEG of the given size.
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func TestAssemblePDF_ValidMultiPage(t *testing.T) {
	pages := [][]byte{jpegBytes(t, 40, 30), jpegBytes(t, 20, 50), jpegBytes(t, 60, 60)}
	var out bytes.Buffer
	if err := AssemblePDF(pages, &out); err != nil {
		t.Fatalf("AssemblePDF: %v", err)
	}
	b := out.Bytes()
	if !bytes.HasPrefix(b, []byte("%PDF")) {
		t.Errorf("output does not start with PDF header, got %q", string(b[:min(8, len(b))]))
	}
	if !bytes.Contains(b, []byte("%%EOF")) {
		t.Error("output missing EOF trailer")
	}
	// One /Type /Page per image (the /Pages tree node is /Type /Pages, not /Page).
	if n := bytes.Count(b, []byte("/Type /Page\n")) + bytes.Count(b, []byte("/Type /Page ")); n < len(pages) {
		// fpdf's exact spacing varies; fall back to counting MediaBox occurrences.
		if mb := bytes.Count(b, []byte("/MediaBox")); mb != len(pages) {
			t.Errorf("MediaBox count = %d, want %d (one page per image)", mb, len(pages))
		}
	}
}

func TestAssemblePDF_Empty(t *testing.T) {
	var out bytes.Buffer
	if err := AssemblePDF(nil, &out); err == nil {
		t.Error("expected error for empty page set")
	}
}

func TestAssemblePDF_BadJPEG(t *testing.T) {
	var out bytes.Buffer
	if err := AssemblePDF([][]byte{[]byte("not a jpeg")}, &out); err == nil {
		t.Error("expected error for invalid jpeg")
	}
}
