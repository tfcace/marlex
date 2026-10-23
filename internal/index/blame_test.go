package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gitBlame returns, per line, the commit git attributes it to.
func gitBlame(r *repo, path string) []string {
	out := r.git("-c", "diff.algorithm=myers", "blame", "--first-parent", "--porcelain", "HEAD", "--", path)
	var commits []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		// Header lines: <40-hex> <orig line> <final line> [<group size>]
		if len(f) >= 3 && len(f[0]) == 40 && !strings.HasPrefix(l, "\t") {
			commits = append(commits, f[0])
		}
	}
	return commits
}

func lines(n int, tag string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d %s\n", i, tag)
	}
	return b.String()
}

func TestBlameMatchesGit(t *testing.T) {
	r := newRepo(t)
	r.write("pkg/f.txt", lines(30, "v0"))
	r.write("other.txt", "a\nb\nc\n")
	r.commit("add f")

	// Edit some lines in the middle, delete a block, insert a block.
	s := strings.Split(lines(30, "v0"), "\n")
	s[4] = "line 5 v1"
	s = append(s[:10], s[14:]...) // delete 11..14
	s = append(s[:20], append([]string{"inserted A", "inserted B"}, s[20:]...)...)
	r.write("pkg/f.txt", strings.Join(s, "\n"))
	r.commit("edit f")

	// Rename with a modification.
	r.git("mv", "pkg/f.txt", "pkg/g.txt")
	cur := r.git("show", "HEAD:pkg/f.txt")
	r.write("pkg/g.txt", "header\n"+cur+"\n")
	r.commit("rename f to g")

	// A branch edits the top, main edits the bottom, then merge.
	r.git("checkout", "-q", "-b", "side")
	g := strings.Split(r.git("show", "HEAD:pkg/g.txt"), "\n")
	g[2] = "side edit"
	r.write("pkg/g.txt", strings.Join(g, "\n")+"\n")
	r.commit("side edit")
	r.git("checkout", "-q", "main")
	g = strings.Split(r.git("show", "HEAD:pkg/g.txt"), "\n")
	g[len(g)-3] = "main edit"
	r.write("pkg/g.txt", strings.Join(g, "\n")+"\n")
	r.commit("main edit")
	r.n++
	r.git("merge", "-q", "--no-ff", "-m", "merge side", "side")

	// Remove the trailing newline: a "\ No newline at end of file" hunk.
	r.write("other.txt", "a\nB\nc")
	r.commit("no newline")

	dir := t.TempDir()
	if _, err := Build(r.dir, dir, BuildOptions{Hunks: true}); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	head, _ := ix.ResolveCommit(r.dir, "HEAD")
	for _, path := range []string{"pkg/g.txt", "other.txt"} {
		want := gitBlame(r, path)
		distinct := map[string]bool{}
		for _, h := range want {
			distinct[h] = true
		}
		if path == "pkg/g.txt" && (len(want) < 25 || len(distinct) < 5) {
			t.Fatalf("test repo too simple: %d lines from %d commits", len(want), len(distinct))
		}
		t.Logf("%s: %d lines attributed to %d distinct commits", path, len(want), len(distinct))
		if n, err := ix.LineCount(path, head); err != nil || n != len(want) {
			t.Errorf("%s: LineCount = %d, %v; git has %d lines", path, n, err, len(want))
		}
		got, err := ix.Blame(path, head, len(want))
		if err != nil {
			t.Fatal(err)
		}
		for i, b := range got {
			if h := ix.Hash(b.Commit).String(); h != want[i] || b.Boundary {
				t.Errorf("%s line %d: mx %s (boundary %v), git %s", path, i+1, h[:8], b.Boundary, want[i][:8])
			}
		}
	}
}

func TestBlameNeedsHunks(t *testing.T) {
	r := history(t)
	dir := t.TempDir()
	if _, err := Build(r.dir, dir, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	ix, _ := Open(dir)
	defer ix.Close()
	head, _ := ix.ResolveCommit(r.dir, "HEAD")
	if _, err := ix.Blame("README.md", head, 1); err != ErrNoHunks {
		t.Errorf("blame without hunks: err = %v, want ErrNoHunks", err)
	}
	ix.Close()
	// Asking for hunks on an index without them rebuilds it.
	if _, err := Build(r.dir, dir, BuildOptions{Hunks: true}); err != nil {
		t.Fatal(err)
	}
	ix, _ = Open(dir)
	if !ix.HasHunks() {
		t.Error("index should have hunks after build --hunks")
	}
	if _, err := ix.Blame("README.md", head, 1); err != nil {
		t.Errorf("blame after adding hunks: %v", err)
	}
}

// Sidecars are mapped on first use. A reader opened before a compaction
// must still blame after the compaction deleted the files it had not mapped
// yet, and concurrent first uses must share one mapping.
func TestLazySidecarsSurviveCompaction(t *testing.T) {
	r := history(t)
	dir := t.TempDir()
	if _, err := Build(r.dir, dir, BuildOptions{Hunks: true, NoCompact: true}); err != nil {
		t.Fatal(err)
	}
	r.write("README.md", "hello\nagain\n")
	r.commit("delta")
	if _, err := Build(r.dir, dir, BuildOptions{NoCompact: true}); err != nil {
		t.Fatal(err)
	}
	held, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if len(held.hunks) != 0 {
		t.Fatal("hunks were mapped at open")
	}
	before := held.Manifest.Segments
	if err := Compact(dir); err != nil {
		t.Fatal(err)
	}
	for _, s := range before {
		if _, err := os.Stat(filepath.Join(dir, s.Hunks)); !os.IsNotExist(err) {
			t.Fatalf("compaction left %s behind", s.Hunks)
		}
	}

	head, _ := held.ResolveCommit(r.dir, "HEAD")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := held.LineCount("README.md", head)
			if err == nil && n != 2 {
				err = fmt.Errorf("LineCount = %d, want 2", n)
			}
			if err == nil {
				_, err = held.Blame("README.md", head, n)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Previous names the line a change replaced, and says when it replaced
// nothing: the history of the line itself, not of its line number.
func TestPreviousFollowsTheLine(t *testing.T) {
	r := newRepo(t)
	r.write("f.txt", "a\nb\nc\n")
	r.commit("add f")
	// Insert two lines at the top and rewrite b: b's line number moves from
	// 2 to 4, and its new version replaced it.
	r.write("f.txt", "x\ny\na\nB\nB2\nc\n")
	r.commit("edit f")
	dir := filepath.Join(r.dir, ".git", "marlex")
	if _, err := Build(r.dir, dir, BuildOptions{Hunks: true}); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	head, _ := ix.ResolveCommit(r.dir, "HEAD")
	bl, err := ix.Blame("f.txt", head, 6)
	if err != nil {
		t.Fatal(err)
	}

	if p, err := ix.Previous("f.txt", bl[3].Commit, bl[3].Line); err != nil || p.Added || p.OldLine != 2 {
		t.Errorf("B replaced line 2 (b): got %+v, %v", p, err)
	}
	if p, err := ix.Previous("f.txt", bl[4].Commit, bl[4].Line); err != nil || !p.Added || p.OldStart != 2 || p.OldLen != 1 {
		t.Errorf("B2 was added past the one line its change rewrote: got %+v, %v", p, err)
	}
	if p, err := ix.Previous("f.txt", bl[0].Commit, bl[0].Line); err != nil || !p.Added {
		t.Errorf("x was added without replacing anything: got %+v, %v", p, err)
	}
	if p, err := ix.Previous("f.txt", bl[2].Commit, bl[2].Line); err != nil || !p.Created {
		t.Errorf("a was born with the file: got %+v, %v", p, err)
	}
}
