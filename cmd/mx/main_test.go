package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the real binary against a scripted repository and check
// what a user sees: output, warnings and exit codes. They pin down the rule
// that a query never answers wrong without saying so.

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mx-bin")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "mx")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	stdout, stderr string
	code           int
}

func run(t *testing.T, dir string, args ...string) result {
	t.Helper()
	return runWith(t, dir, "", nil, args...)
}

// runWith runs mx with stdin and extra environment. Status reports
// are off, so tests never write to the terminal running them.
func runWith(t *testing.T, dir, stdin string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(append(os.Environ(), "MX_NO_REFRESH=", "MX_STATUS=0"), env...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{out.String(), errb.String(), code}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ann Author", "GIT_AUTHOR_EMAIL=ann@example.com",
		"GIT_COMMITTER_NAME=Ann Author", "GIT_COMMITTER_EMAIL=ann@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, path, content string) {
	t.Helper()
	p := filepath.Join(dir, path)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, msg string) {
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", msg)
}

// fixture: pkg/shell/shell.go with three commits, a rename of old.go to
// new.go, and a built index.
func fixture(t *testing.T) string {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "pkg/shell/shell.go", "package shell\n")
	write(t, dir, "pkg/shell/old.go", strings.Repeat("old line\n", 20))
	write(t, dir, "README.md", "hi\n")
	commit(t, dir, "add shell")
	write(t, dir, "pkg/shell/shell.go", "package shell\n\nfunc A() {}\n")
	commit(t, dir, "edit shell")
	gitT(t, dir, "mv", "pkg/shell/old.go", "pkg/shell/new.go")
	commit(t, dir, "rename old to new")
	if r := run(t, dir, "build", "--hunks"); r.code != 0 {
		t.Fatalf("build: %+v", r)
	}
	return dir
}

func lines(s string) int { return len(strings.Split(strings.TrimSpace(s), "\n")) }

func TestPathsResolveLikeGit(t *testing.T) {
	dir := fixture(t)
	sub := filepath.Join(dir, "pkg", "shell")

	// Relative to the current directory, as with git.
	if r := run(t, sub, "log", "shell.go"); r.code != 0 || lines(r.stdout) != 2 {
		t.Errorf("log shell.go from pkg/shell: %+v", r)
	}
	// Repo-relative from a subdirectory still works (agents pass these).
	if r := run(t, sub, "log", "pkg/shell/shell.go"); r.code != 0 || lines(r.stdout) != 2 {
		t.Errorf("repo-relative path from a subdirectory: %+v", r)
	}
	// A directory is the union of its files' histories.
	if r := run(t, dir, "log", "pkg/shell"); r.code != 0 || lines(r.stdout) != 3 {
		t.Errorf("log of a directory: %+v", r)
	}
	if r := run(t, sub, "log", "."); r.code != 0 || lines(r.stdout) != 3 {
		t.Errorf("log . from a subdirectory: %+v", r)
	}
	// --follow crosses the rename.
	if r := run(t, dir, "log", "--follow", "pkg/shell/new.go"); r.code != 0 || lines(r.stdout) != 2 || !strings.Contains(r.stdout, "[pkg/shell/old.go]") {
		t.Errorf("follow: %+v", r)
	}
}

func TestUnknownPathsAreErrors(t *testing.T) {
	dir := fixture(t)
	for _, c := range []struct{ arg, suggestion string }{
		{"pkg/shel/shell.go", "pkg/shell/shell.go"},
		{"pkg/shell/shel.go", "pkg/shell/shell.go"},
		{"pkg/shel", "pkg/shell"},
	} {
		r := run(t, dir, "log", c.arg)
		if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "did you mean "+c.suggestion+"?") {
			t.Errorf("log %s: %+v", c.arg, r)
		}
	}
	if r := run(t, dir, "log", "nothing/like/this.txt"); r.code != 1 || !strings.Contains(r.stderr, "has no history") {
		t.Errorf("unknown path without a suggestion: %+v", r)
	}
	if r := run(t, dir, "log", "--follow", "pkg/shell"); r.code != 1 || !strings.Contains(r.stderr, "directory") {
		t.Errorf("--follow on a directory: %+v", r)
	}
	for _, cmd := range []string{"blame", "cochange"} {
		if r := run(t, dir, cmd, "pkg/shell"); r.code != 1 {
			t.Errorf("%s on a directory should fail: %+v", cmd, r)
		}
	}
}

func TestQueriesFollowHEAD(t *testing.T) {
	dir := fixture(t)
	write(t, dir, "pkg/shell/shell.go", "package shell\n\nfunc B() {}\n")
	commit(t, dir, "after the build")
	head := gitT(t, dir, "rev-parse", "HEAD")

	// --no-refresh answers from the index but says it is behind.
	r := run(t, dir, "--no-refresh", "log", "pkg/shell/shell.go")
	if r.code != 0 || !strings.Contains(r.stderr, "not indexed yet") || !strings.Contains(r.stderr, "mx build includes it") || strings.Contains(r.stdout, "after the build") {
		t.Errorf("no-refresh: %+v", r)
	}
	// By default the index catches up first, and says so.
	r = run(t, dir, "show", "HEAD")
	if r.code != 0 || !strings.Contains(r.stdout, head) || !strings.Contains(r.stderr, "indexed 1 new commit") {
		t.Errorf("auto-refresh: %+v", r)
	}
	// Moving HEAD back to an indexed commit needs no build to be right.
	gitT(t, dir, "checkout", "-q", "HEAD~1")
	r = run(t, dir, "--no-refresh", "log", "-n", "1", "pkg/shell/shell.go")
	if r.code != 0 || strings.Contains(r.stdout, "after the build") || r.stderr != "" {
		t.Errorf("detached at an older commit: %+v", r)
	}
}

func TestShallowCloneUsesPulledIndex(t *testing.T) {
	dir := fixture(t)
	bucket := t.TempDir()
	if r := run(t, dir, "publish", bucket); r.code != 0 {
		t.Fatalf("publish: %+v", r)
	}
	shallow := filepath.Join(t.TempDir(), "shallow")
	gitT(t, dir, "clone", "-q", "--depth", "1", "file://"+dir, shallow)

	if r := run(t, shallow, "log", "README.md"); r.code != 1 || !strings.Contains(r.stderr, "mx pull") {
		t.Errorf("before pull: %+v", r)
	}
	if r := run(t, shallow, "build"); r.code != 1 || !strings.Contains(r.stderr, "shallow") {
		t.Errorf("build in a shallow clone must refuse: %+v", r)
	}
	if r := run(t, shallow, "pull", "--hunks", bucket); r.code != 0 {
		t.Fatalf("pull: %+v", r)
	}
	if r := run(t, shallow, "log", "--follow", "pkg/shell/new.go"); r.code != 0 || lines(r.stdout) != 2 {
		t.Errorf("follow in a shallow clone: %+v", r)
	}
	if r := run(t, shallow, "blame", "--hashes", "pkg/shell/shell.go"); r.code != 0 || lines(r.stdout) != 3 {
		t.Errorf("blame in a shallow clone: %+v", r)
	}
}

func TestEveryQuerySpeaksJSON(t *testing.T) {
	dir := fixture(t)
	for _, args := range [][]string{
		{"log", "pkg/shell"},
		{"show", "HEAD"},
		{"log", "--since", "100y"},
		{"hotspots", "--since", "100y"},
		{"owners", "pkg"},
		{"cochange", "pkg/shell/shell.go"},
		{"blame", "pkg/shell/shell.go"},
		{"stats"},
	} {
		r := run(t, dir, append(args, "--json")...)
		var v any
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &v) != nil {
			t.Errorf("%v --json: %+v", args, r)
		}
	}
	// Text output names authors without their address.
	if r := run(t, dir, "log", "pkg/shell/shell.go"); strings.Contains(r.stdout, "ann@example.com") || !strings.Contains(r.stdout, "Ann Author") {
		t.Errorf("text log author: %q", r.stdout)
	}
}

// log without a path is every commit, newest first; --since cuts either
// form by date or age, before -n.
func TestLogSince(t *testing.T) {
	dir := fixture(t)
	if r := run(t, dir, "log"); r.code != 0 || lines(r.stdout) != 3 || !strings.Contains(strings.Split(r.stdout, "\n")[0], "rename old to new") {
		t.Errorf("log without a path: %+v", r)
	}
	if r := run(t, dir, "log", "-n", "1", "--since", "100y"); r.code != 0 || lines(r.stdout) != 1 {
		t.Errorf("log -n 1 --since: %+v", r)
	}
	if r := run(t, dir, "log", "--since", "100y", "pkg/shell/shell.go"); r.code != 0 || lines(r.stdout) != 2 {
		t.Errorf("log --since with a path: %+v", r)
	}
	if r := run(t, dir, "log", "--since", "2100-01-01"); r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "since 2100-01-01") {
		t.Errorf("log --since in the future: %+v", r)
	}
	if r := run(t, dir, "log", "--since", "soon"); r.code != 1 || !strings.Contains(r.stderr, "bad date") {
		t.Errorf("log --since with a bad date: %+v", r)
	}
	if r := run(t, dir, "log", "--follow"); r.code != 1 {
		t.Errorf("log --follow needs a file: %+v", r)
	}
}

// The CI contract: the index flows down from the full clone to depth-1
// jobs. Every step in a depth-1 pull-request checkout either works or
// refuses with what to do.
func TestDepthOnePullRequestJob(t *testing.T) {
	dir := fixture(t)
	bucket := t.TempDir()
	if r := run(t, dir, "publish", bucket); r.code != 0 {
		t.Fatalf("publish: %+v", r)
	}
	// The pull request's commit exists on the server, after the publish.
	gitT(t, dir, "checkout", "-q", "-b", "pr")
	write(t, dir, "pkg/shell/shell.go", "package shell\n\nfunc C() {}\n")
	commit(t, dir, "pull request change")
	gitT(t, dir, "checkout", "-q", "main")
	pr := filepath.Join(t.TempDir(), "pr")
	gitT(t, dir, "clone", "-q", "--depth", "1", "--branch", "pr", "file://"+dir, pr)
	// Like a GitHub pull_request checkout: depth 1, detached, no branches.
	gitT(t, pr, "checkout", "-q", "--detach")
	gitT(t, pr, "branch", "-D", "-q", "pr")
	if r := run(t, pr, "pull", bucket); r.code != 0 {
		t.Fatalf("pull: %+v", r)
	}

	r := run(t, pr, "log", "pkg/shell/shell.go")
	if r.code != 0 || lines(r.stdout) != 2 || !strings.Contains(r.stderr, "answers cover history up to its parent") || strings.Contains(r.stderr, "pull a newer index") {
		t.Errorf("log in a PR job: %+v", r)
	}
	if r := run(t, pr, "show", "HEAD"); r.code != 1 || !strings.Contains(r.stderr, "not in the index yet") {
		t.Errorf("show HEAD must not answer about another commit: %+v", r)
	}
	if r := run(t, pr, "order"); r.code != 1 || !strings.Contains(r.stderr, "--files") {
		t.Errorf("order without a visible base must refuse: %+v", r)
	}
	if r := run(t, pr, "publish", t.TempDir()); r.code != 1 || !strings.Contains(r.stderr, "publish from the full clone") {
		t.Errorf("publish of a pulled index must refuse: %+v", r)
	}
}

// mx ff: a depth-1 checkout of a branch, with an index published from
// main only, fetches just enough history and indexes the branch on top.
func TestFastForwardBranchOnPulledIndex(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	origin := t.TempDir()
	gitT(t, origin, "init", "-q", "-b", "main")
	write(t, origin, "go.mod", "module example.com/m\n\ngo 1.22\n")
	for _, p := range []string{"a", "b"} {
		write(t, origin, p+"/"+p+".go", "package "+p+"\n")
		write(t, origin, p+"/"+p+"_test.go", "package "+p+"\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")
	}
	commit(t, origin, "init")
	write(t, origin, "README.md", "one\n")
	commit(t, origin, "main one")
	// The branch starts here; main moves on and is merged into it midway.
	gitT(t, origin, "checkout", "-q", "-b", "feature")
	write(t, origin, "b/b.go", "package b\n// branch one\n")
	commit(t, origin, "branch one")
	gitT(t, origin, "checkout", "-q", "main")
	write(t, origin, "a/a.go", "package a\n// main moves on\n")
	commit(t, origin, "main two")
	gitT(t, origin, "checkout", "-q", "feature")
	gitT(t, origin, "merge", "-q", "--no-edit", "main")
	write(t, origin, "b/b.go", "package b\n// branch one\n// branch two\n")
	commit(t, origin, "branch two")
	gitT(t, origin, "checkout", "-q", "main")

	// Post-merge job: an index of main only, published.
	mainClone := filepath.Join(t.TempDir(), "main")
	gitT(t, origin, "clone", "-q", "--single-branch", "--branch", "main", "file://"+origin, mainClone)
	bucket := t.TempDir()
	if r := run(t, mainClone, "build", "--hunks"); r.code != 0 {
		t.Fatalf("build: %+v", r)
	}
	if r := run(t, mainClone, "publish", bucket); r.code != 0 {
		t.Fatalf("publish: %+v", r)
	}

	// Pull-request job: depth 1, detached, no branches.
	pr := filepath.Join(t.TempDir(), "pr")
	gitT(t, origin, "clone", "-q", "--depth", "1", "--branch", "feature", "file://"+origin, pr)
	gitT(t, pr, "checkout", "-q", "--detach")
	gitT(t, pr, "branch", "-D", "-q", "feature")
	if r := run(t, pr, "pull", "--hunks", bucket); r.code != 0 {
		t.Fatalf("pull: %+v", r)
	}
	if r := run(t, pr, "log", "b/b.go"); r.code != 0 || !strings.Contains(r.stderr, "mx ff") {
		t.Errorf("before ff, queries should point at ff: %+v", r)
	}
	if r := run(t, pr, "ff"); r.code != 0 || !strings.Contains(r.stdout, "indexed 3 branch commit(s)") {
		t.Fatalf("ff: %+v", r)
	}
	if r := run(t, pr, "show", "HEAD"); r.code != 0 || !strings.Contains(r.stdout, "branch two") {
		t.Errorf("show HEAD after ff: %+v", r)
	}
	if r := run(t, pr, "blame", "--hashes", "b/b.go"); r.code != 0 || lines(r.stdout) != 3 {
		t.Errorf("blame after ff: %+v", r)
	}
	// The change is the branch's own (b), not main's change it merged (a).
	r := run(t, pr, "order", "--list")
	if want := "example.com/m/b\nexample.com/m/a\n"; r.code != 0 || r.stdout != want {
		t.Errorf("order after ff:\n got %+v\nwant %q", r, want)
	}
	if r := run(t, pr, "publish", t.TempDir()); r.code != 1 {
		t.Errorf("publish of a pulled index must refuse: %+v", r)
	}
	// Even after unshallowing, an index with local branch commits stays local.
	gitT(t, pr, "fetch", "-q", "--unshallow", "origin")
	if r := run(t, pr, "publish", t.TempDir()); r.code != 1 || !strings.Contains(r.stderr, "mx ff") {
		t.Errorf("publish of an index holding ff commits must refuse: %+v", r)
	}
}
