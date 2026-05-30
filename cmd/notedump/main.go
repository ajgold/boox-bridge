// notedump parses a Boox .note file and prints a summary of pages, shapes,
// and stroke points. Throwaway debug command used to validate parser
// compatibility against real device exports (Note Air4C, Note Air 5c, etc.).
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sysop/ultrabridge/internal/booxnote"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: notedump <file.note>")
		os.Exit(2)
	}
	path := os.Args[1]
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stat: %v\n", err)
		os.Exit(1)
	}

	note, err := booxnote.Open(f, stat.Size())
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("File:   %s (%d bytes)\n", path, stat.Size())
	fmt.Printf("NoteID: %s\n", note.NoteID)
	fmt.Printf("Title:  %q\n", note.Title)
	fmt.Printf("Pages:  %d\n", len(note.Pages))

	totalShapes, totalPoints, withText := 0, 0, 0
	for i, page := range note.Pages {
		shapePoints := 0
		for _, s := range page.Shapes {
			shapePoints += len(s.Points)
			if s.Text != "" {
				withText++
			}
		}
		totalShapes += len(page.Shapes)
		totalPoints += shapePoints
		fmt.Printf("  Page %d: id=%s size=%.0fx%.0f shapes=%d points=%d\n",
			i+1, page.PageID, page.Width, page.Height, len(page.Shapes), shapePoints)
	}
	fmt.Printf("\nTotal shapes: %d\nTotal points: %d\nShapes with embedded text: %d\n",
		totalShapes, totalPoints, withText)

	// Dump first shape on first page for format inspection
	if len(note.Pages) > 0 && len(note.Pages[0].Shapes) > 0 {
		s := note.Pages[0].Shapes[0]
		var firstPoint any
		if len(s.Points) > 0 {
			firstPoint = s.Points[0]
		}
		b, _ := json.MarshalIndent(struct {
			UniqueID    string
			ShapeType   int32
			Color       string
			Thickness   float32
			ZOrder      int32
			Text        string
			PointsCount int
			FirstPoint  any
		}{
			UniqueID:    s.UniqueID,
			ShapeType:   s.ShapeType,
			Color:       fmt.Sprintf("0x%08X", uint32(s.Color)),
			Thickness:   s.Thickness,
			ZOrder:      s.ZOrder,
			Text:        s.Text,
			PointsCount: len(s.Points),
			FirstPoint:  firstPoint,
		}, "", "  ")
		fmt.Println("\nFirst shape on first page:")
		fmt.Println(string(b))
	}
}
