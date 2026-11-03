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

func TestCountTerms(t *testing.T) {
	texts := map[string]string{
		"a": "I run CNHL packs. cnhl is great, Ovonic is fine",
		"b": "Ovonic 6000 here",
		"c": "nothing relevant",
	}
	got := countTerms(texts, []string{"CNHL", "Ovonic", "Zeee"})
	if got[0].Term != "Ovonic" || got[0].Threads != 2 || got[0].Mentions != 2 {
		t.Errorf("top row wrong: %+v", got[0])
	}
	if got[1].Term != "CNHL" || got[1].Threads != 1 || got[1].Mentions != 2 {
		t.Errorf("case-insensitive count wrong: %+v", got[1])
	}
	if got[2].Threads != 0 {
		t.Errorf("unmentioned term should count zero: %+v", got[2])
	}
}

func TestSearchTarget(t *testing.T) {
	if got := searchTarget("a b", "r/arrma", "top", "year"); !strings.Contains(got, "/r/arrma/search/") || !strings.Contains(got, "restrict_sr=1") {
		t.Errorf("sub search url wrong: %s", got)
	}
	if got := searchTarget("x", "", "", ""); got != "https://www.reddit.com/search/?q=x" {
		t.Errorf("plain search url wrong: %s", got)
	}
}
