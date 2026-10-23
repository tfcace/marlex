package rank

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tfcace/marlex/internal/index"
)

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ann", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=Ann",
		"GIT_COMMITTER_EMAIL=a@x", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func write(t *testing.T, dir string, files map[string]string) {
	for p, c := range files {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func commit(t *testing.T, dir, msg string) {
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-q", "-m", msg)
}

func names(rs []Ranked) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Unit)
	}
	return out
}

// Module layout: b imports a; c and d are independent. c's test always
// changes together with util/util.go.
func goRepo(t *testing.T) string {
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	write(t, dir, map[string]string{
		"go.mod":         "module example.com/m\n\ngo 1.22\n",
		"a/a.go":         "package a\n\nfunc A() int { return 1 }\n",
		"a/a_test.go":    "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"b/b.go":         "package b\n\nimport \"example.com/m/a\"\n\nfunc B() int { return a.A() }\n",
		"b/b_test.go":    "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n",
		"c/c.go":         "package c\n",
		"c/c_test.go":    "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n",
		"d/d.go":         "package d\n",
		"d/d_test.go":    "package d\n\nimport \"testing\"\n\nfunc TestD(t *testing.T) {}\n",
		"util/util.go":   "package util\n",
		"util/README.md": "util\n",
	})
	commit(t, dir, "init")
	for i := range 3 {
		write(t, dir, map[string]string{
			"util/util.go": "package util\n// v" + string(rune('0'+i)) + "\n",
			"c/c_test.go":  "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n// v" + string(rune('0'+i)) + "\n",
		})
		commit(t, dir, "util and c")
	}
	return dir
}

func TestRankGo(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	repo := goRepo(t)
	idx := t.TempDir()
	if _, err := index.Build(repo, idx, index.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	ix, err := index.Open(idx)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	// Changing a: a (direct), then b (imports a), then c and d, which have
	// no signal and sort by name.
	rs, err := Rank(ix, repo, []Adapter{GoAdapter{}}, []string{"a/a.go"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"example.com/m/a", "example.com/m/b", "example.com/m/c", "example.com/m/d"}
	if got := names(rs); !slices.Equal(got, want) {
		t.Errorf("order for a/a.go:\n got %v\nwant %v", got, want)
	}
	if rs[1].Distance != 1 {
		t.Errorf("b distance = %d, want 1", rs[1].Distance)
	}

	// Changing util: c co-changed with it every time (init plus three
	// edits), so c outranks the statically unrelated a, b and d.
	rs, _ = Rank(ix, repo, []Adapter{GoAdapter{}}, []string{"util/util.go"}, Options{})
	if got := names(rs); got[0] != "example.com/m/c" {
		t.Errorf("order for util/util.go = %v", got)
	}
	if rs[0].CoChange <= 0 || !strings.Contains(strings.Join(rs[0].Reasons, ";"), "co-changed with util/util.go in 4 of 4") {
		t.Errorf("c reasons = %v", rs[0].Reasons)
	}

	// With nothing changed there is no signal, and units sort by name.
	rs, _ = Rank(ix, repo, []Adapter{GoAdapter{}}, nil, Options{})
	if got := names(rs); !slices.Equal(got, want) {
		t.Errorf("order with no change = %v", got)
	}
}

func TestFilesAdapter(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "git", "init", "-q", "-b", "main")
	write(t, repo, map[string]string{
		"src/math.ts":             "export const add = (a: number, b: number) => a + b\n",
		"src/format.ts":           "import { add } from './math'\nexport const f = () => String(add(1, 2))\n",
		"src/format.test.ts":      "import { f } from './format'\ntest('f', () => {})\n",
		"src/other.test.ts":       "test('o', () => {})\n",
		"pkg/__init__.py":         "",
		"pkg/core.py":             "def go(): pass\n",
		"pkg/api.py":              "from pkg.core import go\n",
		"tests/test_api.py":       "from pkg import api\n",
		"tests/test_unrelated.py": "def test_x(): pass\n",
	})
	units, err := FilesAdapter{}.Units(repo, []string{"src/math.ts", "pkg/core.py"})
	if err != nil {
		t.Fatal(err)
	}
	dist := map[string]int{}
	for _, u := range units {
		dist[u.Name] = u.Distance
	}
	want := map[string]int{
		"src/format.test.ts":      2, // test -> format -> math
		"src/other.test.ts":       -1,
		"tests/test_api.py":       2, // test -> pkg.api -> pkg.core
		"tests/test_unrelated.py": -1,
	}
	for k, v := range want {
		if dist[k] != v {
			t.Errorf("distance %s = %d, want %d", k, dist[k], v)
		}
	}
}

// A nested module that can't load (teleport's examples/go-client needs its
// parent's build to resolve) is left out; the rest still ranks.
func TestRankGoSkipsBrokenModule(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	repo := goRepo(t)
	// As in teleport: a go 1.15 module (no graph pruning) using a local
	// module whose own requirement has no go.sum entry here.
	write(t, repo, map[string]string{
		"examples/lib/go.mod":     "module example.com/lib\n\ngo 1.15\n\nrequire golang.org/x/text v0.3.0\n",
		"examples/lib/lib.go":     "package lib\n",
		"examples/client/go.mod":  "module example.com/client\n\ngo 1.15\n\nrequire example.com/lib v0.0.0\n\nreplace example.com/lib => ../lib\n",
		"examples/client/main.go": "package main\n\nimport _ \"example.com/lib\"\n\nfunc main() {}\n",
	})
	commit(t, repo, "add a broken example module")
	idx := t.TempDir()
	if _, err := index.Build(repo, idx, index.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	ix, err := index.Open(idx)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	rs, err := Rank(ix, repo, []Adapter{GoAdapter{}}, []string{"a/a.go"}, Options{})
	if err != nil {
		t.Fatalf("a broken nested module stopped the ranking: %v", err)
	}
	if got := names(rs); len(got) < 2 || got[0] != "example.com/m/a" {
		t.Errorf("order with a broken example module = %v", got)
	}
}
