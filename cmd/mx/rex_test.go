package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// why answers for a file:line given directly, and for one found on a saved
// terminal screen, nearest the cursor, written the way go test writes it.
func TestWhyCard(t *testing.T) {
	dir := fixture(t)

	r := run(t, dir, "why", "--json", "pkg/shell/shell.go:3")
	var card struct {
		Line   int    `json:"line"`
		Text   string `json:"text"`
		Commit struct {
			Subject string `json:"subject"`
		} `json:"commit"`
		History []struct {
			Subject string `json:"subject"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &card); err != nil || r.code != 0 {
		t.Fatalf("why --json: %v %+v", err, r)
	}
	if card.Commit.Subject != "edit shell" || card.Text != "func A() {}" || len(card.History) != 2 {
		t.Errorf("why --json: %+v", card)
	}

	r = run(t, dir, "why", "pkg/shell/shell.go:3")
	for _, want := range []string{"[ PKG/SHELL/SHELL.GO:3 ]", "3 │ func A() {}", "● edit shell", "[ HISTORY ]"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("why card lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "\x1b") {
		t.Errorf("escape sequences in piped output")
	}

	scr := filepath.Join(t.TempDir(), "screen.txt")
	os.WriteFile(scr, []byte("$ go test ./...\n--- FAIL: TestA\n    shell.go:3: boom\nFAIL\n    README.md is fine\n$ \n"), 0o644)
	r = run(t, filepath.Join(dir, "pkg"), "why", "--json", "--screen", scr, "--cursor", "4,1", "--rows", "6")
	if !strings.Contains(r.stdout, `"path": "pkg/shell/shell.go"`) || !strings.Contains(r.stdout, `"line": 3`) {
		t.Errorf("why --screen picked the wrong reference: %+v", r)
	}
	// An editor: its buffer tabs name files, and its status line names the
	// file it shows and the cursor's line, which comes first.
	os.WriteFile(scr, []byte(" README.md  shell.go \n    1  package shell\n    3  func A() {}\n NOR   pkg/shell/shell.go   1 sel  3:1  LF \n"), 0o644)
	r = run(t, dir, "why", "--json", "--screen", scr, "--cursor", "1,1", "--rows", "4", "--program", "hx")
	if !strings.Contains(r.stdout, `"path": "pkg/shell/shell.go"`) || !strings.Contains(r.stdout, `"line": 3`) {
		t.Errorf("why in an editor: %+v", r)
	}
	os.WriteFile(scr, []byte("nothing to see at 1.2.3 or io.Writer\n"), 0o644)
	if r = run(t, dir, "why", "--screen", scr); r.code != 1 || !strings.Contains(r.stderr, "has history") {
		t.Errorf("why with nothing on screen: %+v", r)
	}
}

// rex install writes the module with this binary's path and loads it from
// init.lua once, keeping what init.lua already had.
func TestRexInstall(t *testing.T) {
	cfg := t.TempDir()
	write(t, cfg, "init.lua", `rex.bind("cmd+e", "pane.zoom")`)
	env := []string{"PATH=/usr/bin:/bin", "REX_CONFIG="} // no rex to reload
	for i := 0; i < 2; i++ {
		if r := runWith(t, cfg, "", env, "rex", "install", "--dir", cfg, "--key", "cmd+shift+y"); r.code != 0 {
			t.Fatalf("rex install: %+v", r)
		}
	}
	init, _ := os.ReadFile(filepath.Join(cfg, "init.lua"))
	if !strings.HasPrefix(string(init), `rex.bind("cmd+e", "pane.zoom")`) || strings.Count(string(init), `require("mx")`) != 1 ||
		!strings.Contains(string(init), `setup{ key = "cmd+shift+y" }`) {
		t.Errorf("init.lua:\n%s", init)
	}
	mod, _ := os.ReadFile(filepath.Join(cfg, "mx.lua"))
	if !strings.Contains(string(mod), `bin = "`+bin+`",`) && !strings.Contains(string(mod), `/mx",`) {
		t.Errorf("mx.lua does not name the binary:\n%.400s", mod)
	}
}
