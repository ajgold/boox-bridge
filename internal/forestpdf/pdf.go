// Package forestpdf assembles a set of JPEG page images into a single PDF, one
// image per page, via the pure-Go go-pdf/fpdf library (no CGO). JPEG data is
// embedded through fpdf's image registry (DCTDecode, no recompression). Used to
// export a ForestNote notebook: each live page is rendered to JPEG (reusing the
// on-the-fly stroke renderer) and handed here.
package forestpdf

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"io"
	"strconv"

	"github.com/go-pdf/fpdf"
)

// AssemblePDF writes a PDF containing one page per input JPEG, in order, to w.
// Each PDF page is sized to its image's pixel dimensions at 72 DPI (1px → 1pt),
// so pages preserve the source aspect ratio. Returns an error for an empty page
// set or any non-JPEG entry.
func AssemblePDF(pages [][]byte, w io.Writer) error {
	if len(pages) == 0 {
		return fmt.Errorf("forestpdf: no pages to assemble")
	}

	pdf := fpdf.New("P", "pt", "A4", "")
	pdf.SetAutoPageBreak(false, 0)

	for i, jpg := range pages {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpg))
		if err != nil {
			return fmt.Errorf("forestpdf: page %d not valid jpeg: %w", i, err)
		}
		wPt, hPt := float64(cfg.Width), float64(cfg.Height)

		// One PDF page per image, sized to the image so nothing is cropped/scaled.
		pdf.AddPageFormat("P", fpdf.SizeType{Wd: wPt, Ht: hPt})

		// Register each image under a distinct name and draw it to fill the page.
		name := "p" + strconv.Itoa(i)
		opt := fpdf.ImageOptions{ImageType: "JPEG", ReadDpi: false}
		pdf.RegisterImageOptionsReader(name, opt, bytes.NewReader(jpg))
		pdf.ImageOptions(name, 0, 0, wPt, hPt, false, opt, 0, "")
	}

	if err := pdf.Output(w); err != nil {
		return fmt.Errorf("forestpdf: output: %w", err)
	}
	return nil
}
