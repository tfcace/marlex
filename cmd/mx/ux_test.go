package main

import (
	"strings"
	"testing"
)

// The first question in a repository indexes it, line ranges included at
// this size, and answers; the next one says nothing about indexing.
func TestFirstQueryIndexes(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.go", "package a\n\nfunc A() {}\n")
	commit(t, dir, "add a")
	write(t, dir, "a.go", "package a\n\nfunc A() { println() }\n")
	commit(t, dir, "edit a")

	r := run(t, dir, "why", "a.go:3")
	if r.code != 0 || !strings.Contains(r.stderr, "for the first time") || !strings.Contains(r.stdout, "edit a") {
		t.Fatalf("first why: %+v", r)
	}
	if !strings.Contains(r.stderr, "of history by 1 person") {
		t.Errorf("no summary of what was indexed: %q", r.stderr)
	}
	if r = run(t, dir, "log", "a.go"); r.code != 0 || r.stderr != "" || lines(r.stdout) != 2 {
		t.Errorf("second query: %+v", r)
	}
}

// Asked not to write, mx doesn't build, and says how to.
func TestNoRefreshDoesNotIndex(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.go", "package a\n")
	commit(t, dir, "add a")
	r := runWith(t, dir, "", []string{"MX_NO_REFRESH=1"}, "log", "a.go")
	if r.code != 1 || !strings.Contains(r.stderr, "--no-refresh") || !strings.Contains(r.stderr, "mx build") {
		t.Errorf("log with no index and no refresh: %+v", r)
	}
}

// A question about a line adds line ranges to an index built without them.
func TestLineQuestionAddsLineRanges(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.go", "package a\n\nfunc A() {}\n")
	commit(t, dir, "add a")
	run(t, dir, "build") // core only
	r := run(t, dir, "blame", "a.go")
	if r.code != 0 || !strings.Contains(r.stderr, "adding line ranges") || lines(r.stdout) != 3 {
		t.Errorf("blame without line ranges: %+v", r)
	}
}

// Bare mx greets: what the repository has, and questions to paste.
func TestGreeting(t *testing.T) {
	dir := fixture(t)
	r := run(t, dir)
	if r.code != 0 || !strings.Contains(r.stdout, "commits indexed") || !strings.Contains(r.stdout, "mx why pkg/shell/") ||
		!strings.Contains(r.stdout, "mx help why") {
		t.Errorf("greeting: %+v", r)
	}
	r = run(t, t.TempDir())
	if r.code != 0 || !strings.Contains(r.stdout, "isn't inside a git repository") {
		t.Errorf("greeting outside a repository: %+v", r)
	}
}

// Help is one command's own, and mistakes get a suggestion.
func TestHelpAndTypos(t *testing.T) {
	dir := fixture(t)
	cases := []struct {
		args     []string
		code     int
		out, err string
	}{
		{[]string{"help", "log"}, 0, "examples:", ""},
		{[]string{"help", "rex"}, 0, "--key", ""},
		{[]string{"why", "-h"}, 0, "--json", ""},
		{[]string{"wy", "x"}, 2, "", "did you mean mx why?"},
		{[]string{"log", "--folow", "x"}, 2, "", "did you mean --follow?"},
		{[]string{"log", "-n", "abc", "x"}, 2, "", `-n wants a number, not "abc"`},
		{[]string{"blame"}, 2, "", "e.g. mx blame"},
	}
	for _, c := range cases {
		r := run(t, dir, c.args...)
		if r.code != c.code || !strings.Contains(r.stdout, c.out) || !strings.Contains(r.stderr, c.err) {
			t.Errorf("mx %v: %+v", c.args, r)
		}
	}
	if r := run(t, dir, "why", "-h"); strings.Contains(r.stdout, "--pid") {
		t.Errorf("why's help shows its plumbing flags:\n%s", r.stdout)
	}
}
