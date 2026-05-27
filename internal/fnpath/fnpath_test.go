package fnpath

import "testing"

func TestNotebookID(t *testing.T) {
	cases := map[string]string{
		"forestnote://NB1/PG2": "NB1", // page URI
		"forestnote://NB1":     "NB1", // notebook URI
		"/supernote/foo.note":  "",    // not a ForestNote URI
		"":                     "",
	}
	for in, want := range cases {
		if got := NotebookID(in); got != want {
			t.Errorf("NotebookID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPageRoundTrip(t *testing.T) {
	p := Page("NB1", "PG2")
	if p != "forestnote://NB1/PG2" {
		t.Fatalf("Page = %q", p)
	}
	if !Is(p) {
		t.Error("Is should be true for a page URI")
	}
	if PageID(p) != "PG2" {
		t.Errorf("PageID = %q, want PG2", PageID(p))
	}
	if NotebookID(p) != "NB1" {
		t.Errorf("NotebookID = %q, want NB1", NotebookID(p))
	}
}
