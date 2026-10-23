package screen

import (
	"fmt"
	"testing"
)

func TestFind(t *testing.T) {
	cases := []struct {
		line string
		want []string // path:line
	}{
		{"internal/programstatus/programstatus.go:163:func NewReporter(w io.Writer)", []string{"internal/programstatus/programstatus.go:163"}},
		{"    shell_test.go:42: expected 3, got 4", []string{"shell_test.go:42"}},
		{"/Users/me/src/hash/internal/shell/shell.go:88 +0x1d", []string{"/Users/me/src/hash/internal/shell/shell.go:88"}},
		{`  File "pkg/mod.py", line 12, in run`, []string{"pkg/mod.py:12"}},
		{"src/app.ts(12,5): error TS2322", []string{"src/app.ts:12"}},
		{"  --> src/main.rs:10:5", []string{"src/main.rs:10"}},
		{"diff --git a/README.md b/README.md", []string{"a/README.md:0", "b/README.md:0"}},
		{"see https://github.com/tfcace/hash/blob/main/x.go#L3 for 1.2.3", nil},
		{"Edit(internal/shell/agent_status.go)", []string{"internal/shell/agent_status.go:0"}},
		{"took 0.45s, version 1.22", nil},
	}
	for _, c := range cases {
		var got []string
		for _, r := range Find([]string{c.line}, 0) {
			got = append(got, fmt.Sprintf("%s:%d", r.Path, r.Line))
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%q: got %v, want %v", c.line, got, c.want)
		}
	}
}

func TestFindJoinsWrappedRows(t *testing.T) {
	rows := []string{"error at internal/shell/sh", "ell.go:12 here"}
	refs := Find(rows, 26)
	if len(refs) == 0 || refs[0].Path != "internal/shell/shell.go" || refs[0].Line != 12 || refs[0].Row != 0 {
		t.Fatalf("got %+v", refs)
	}
}

func TestByDistance(t *testing.T) {
	refs := []Ref{
		{Path: "a.go", Line: 1, Row: 1},
		{Path: "b.go", Line: 2, Row: 5},
		{Path: "c.go", Line: 3, Row: 9},
		{Path: "a.go", Line: 1, Row: 6},
	}
	got := ByDistance(refs, 7, 0)
	want := "[a.go b.go c.go]"
	var names []string
	for _, r := range got {
		names = append(names, r.Path)
	}
	if fmt.Sprint(names) != want {
		t.Fatalf("got %v, want %v", names, want)
	}
	if got[0].Row != 6 {
		t.Fatalf("kept the farther duplicate: %+v", got[0])
	}
}

func TestEditorTitle(t *testing.T) {
	cases := []struct{ title, want string }{
		{"/src/hash/internal/shell/shell.go:302", "/src/hash/internal/shell/shell.go:302"},
		{"shell.go (~/src/hash/internal/shell) - VIM", "/home/me/src/hash/internal/shell/shell.go:0"},
		{"shell.go + (~/src/hash/internal/shell) - VIM", "/home/me/src/hash/internal/shell/shell.go:0"},
	}
	screen := []string{"code", "code", "                              302,1         13%"}
	for _, c := range cases {
		r, ok := Editor(screen[:2], 0, c.title, "/home/me")
		if got := fmt.Sprintf("%s:%d", r.Path, r.Line); !ok || got != c.want {
			t.Errorf("title %q: got %s (%v), want %s", c.title, got, ok, c.want)
		}
	}
	// The default title names the file; the ruler gives the line.
	r, _ := Editor(screen, 0, "shell.go (~/src) - VIM", "/home/me")
	if got := fmt.Sprintf("%s:%d", r.Path, r.Line); got != "/home/me/src/shell.go:302" {
		t.Errorf("title and ruler: got %s", got)
	}
}

func TestEditor(t *testing.T) {
	cases := []struct {
		name   string
		screen []string
		cursor int
		want   string
	}{
		{"helix status line", []string{
			"   119      shell := &Shell{",
			"   120      initialPrompt := \"> \"",
			" NOR   internal/shell/shell.go                      master 1 sel  120:1  LF  go ",
			"",
		}, 1, "internal/shell/shell.go:120"},
		{"vim message and gutter", []string{
			" 122         // This avoids spawning",
			" 123         initialPrompt := \"> \"",
			"\"internal/shell/shell.go\" 2275L, 69832B",
		}, 1, "internal/shell/shell.go:123"},
		{"vim status line and ruler", []string{
			"        initialPrompt := \"> \"",
			"internal/shell/shell.go                                     123,9          5%",
			"",
		}, 0, "internal/shell/shell.go:123"},
		{"relative numbers, no ruler", []string{
			"  1 foo()",
			"0   bar()",
			"internal/shell/shell.go",
		}, 1, "internal/shell/shell.go:0"},
		{"micro", []string{"x", "shell.go (88,3) | ft:go | unix | utf-8"}, 0, "shell.go:88"},
		{"vim ruler alone", []string{
			"        initialPrompt := \"> \"",
			"\"internal/shell/shell.go\" 2275L, 69832B                     123,9          5%",
		}, 0, "internal/shell/shell.go:123"},
		{"nano title bar and constantshow", []string{
			"  GNU nano 8.2            internal/shell/shell.go            Modified  ",
			"",
			"func New() {",
			"[ line 42/2275 (1%), col 1/12 (8%), char 1201/69832 (1%) ]",
			"^G Help      ^O Write Out  ^W Where Is",
			"^X Exit      ^R Read File  ^\\ Replace",
		}, 2, "internal/shell/shell.go:42"},
		{"less -M prompt, the top line", []string{
			"package main",
			"",
			"cmd/hash/main.go lines 120-168/320 37%",
		}, 2, "cmd/hash/main.go:120"},
	}
	for _, c := range cases {
		r, ok := Editor(c.screen, c.cursor, "", "/home/me")
		if got := fmt.Sprintf("%s:%d", r.Path, r.Line); !ok || got != c.want {
			t.Errorf("%s: got %s (%v), want %s", c.name, got, ok, c.want)
		}
	}
	if _, ok := Editor([]string{"no files here", "-- INSERT --"}, 0, "", ""); ok {
		t.Error("found a file on a screen with none")
	}
}
