package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func capture(f func()) string {
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestPrintResults(t *testing.T) {
	raw := `[{"sub":"arrma","title":"Mini Kraton review","url":"https://www.reddit.com/r/arrma/comments/abc/x/"},{"sub":"rccars","title":"XT60 swap","url":"https://www.reddit.com/r/rccars/comments/def/y/"}]`
	out := capture(func() { printResults(raw, 1) })
	if !strings.Contains(out, "Mini Kraton review") || strings.Contains(out, "XT60 swap") {
		t.Errorf("limit not honoured:\n%s", out)
	}
	if !strings.Contains(capture(func() { printResults("[]", 5) }), "no results") {
		t.Error("empty result should say so")
	}
}

func TestCleanThreadURL(t *testing.T) {
	u, _ := cleanThreadURL("https://www.reddit.com/r/arrma/comments/abc/title/?utm=1")
	if u != "https://www.reddit.com/r/arrma/comments/abc/title/" {
		t.Errorf("got %q", u)
	}
	if _, err := cleanThreadURL("https://evil.example/r/x/comments/a/b/"); err == nil {
		t.Error("non-reddit host should be refused")
	}
}

func TestClip(t *testing.T) {
	if got := clip(strings.Repeat("a", 50), 10); !strings.Contains(got, "cut at 10") {
		t.Errorf("not clipped: %q", got)
	}
}
