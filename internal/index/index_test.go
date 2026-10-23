package index

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

type repo struct {
	t   *testing.T
	dir string
	n   int
}

func newRepo(t *testing.T) *repo {
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	date := fmt.Sprintf("2026-01-01T00:%02d:00Z", r.n%60)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Ann", "GIT_AUTHOR_EMAIL=ann@example.com", "GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME=Ann", "GIT_COMMITTER_EMAIL=ann@example.com", "GIT_COMMITTER_DATE="+date,
		"GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) write(path, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, path)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.n++
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// history builds a small repository with a rename, a side branch and a merge.
func history(t *testing.T) *repo {
	r := newRepo(t)
	body := strings.Repeat("line of code\n", 20)
	r.write("src/a.go", body)
	r.write("README.md", "hi\n")
	r.commit("add a")
	r.write("src/a.go", body+"more\n")
	r.commit("edit a")
	r.git("mv", "src/a.go", "src/b.go")
	r.commit("rename a to b")
	r.git("checkout", "-q", "-b", "feature")
	r.write("src/b.go", body+"more\nfeature\n")
	r.write("docs/x.md", "x\n")
	r.commit("feature work")
	r.git("checkout", "-q", "main")
	r.write("README.md", "hello\n")
	r.commit("readme")
	r.n++
	r.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	return r
}

func hashesOf(ix *Index, entries []LogEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, ix.Hash(e.Idx).String())
	}
	return out
}

func gitLines(r *repo, args ...string) []string {
	out := r.git(args...)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestBuildAndLog(t *testing.T) {
	r := history(t)
	dir := t.TempDir()
	res, err := Build(r.dir, dir, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.NewCommits != 6 {
		t.Fatalf("indexed %d commits, want 6", res.NewCommits)
	}
	ix, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	// Non-merge history of every path must match git exactly.
	for _, p := range []string{"src/a.go", "src/b.go", "README.md", "docs/x.md"} {
		var got []string
		for _, e := range ix.Log(p, false, nil, 0) {
			if len(ix.Parents(e.Idx)) <= 1 {
				got = append(got, ix.Hash(e.Idx).String())
			}
		}
		want := gitLines(r, "log", "--all", "--full-history", "--no-merges", "--format=%H", "--", p)
		if !slices.Equal(got, want) {
			t.Errorf("log %s:\n got %v\nwant %v", p, got, want)
		}
	}

	// --follow across the rename, scoped to HEAD.
	scope, err := ix.ResolveScope(r.dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	got := hashesOf(ix, ix.Log("src/b.go", true, scope, 0))
	want := gitLines(r, "log", "--follow", "--format=%H", "--", "src/b.go")
	// git's --follow omits the merge (it is TREESAME to its second parent);
	// the index records the merge's first-parent diff, so it appears too.
	merge := r.git("rev-parse", "HEAD")
	got = slices.DeleteFunc(got, func(h string) bool { return h == merge })
	if !slices.Equal(got, want) {
		t.Errorf("follow:\n got %v\nwant %v", got, want)
	}

	// Lookup by prefix and changes.
	idx, err := ix.LookupHash(merge[:8])
	if err != nil {
		t.Fatal(err)
	}
	c := ix.Commit(idx)
	if c.Subject != "merge feature" || len(c.Parents) != 2 {
		t.Errorf("merge commit = %+v", c)
	}
	var paths []string
	for _, ch := range ix.Changes(idx) {
		paths = append(paths, ch.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"docs/x.md", "src/b.go"}) {
		t.Errorf("first-parent changes of merge = %v", paths)
	}

	// The feature branch's own commit is reachable from feature and HEAD;
	// the readme commit is not reachable from feature.
	feature, _ := ix.RefID("feature")
	readme, _ := ix.LookupHash(r.git("rev-parse", "main~1"))
	if ix.Reachable(readme, feature) {
		t.Error("readme commit should not be reachable from feature")
	}
	if kids := ix.Children(readme); len(kids) != 1 || kids[0] != idx {
		t.Errorf("children of readme commit = %v, want [%d]", kids, idx)
	}
}

func TestIncrementalCompactionAndForcePush(t *testing.T) {
	r := history(t)
	dir := t.TempDir()
	if _, err := Build(r.dir, dir, BuildOptions{NoCompact: true}); err != nil {
		t.Fatal(err)
	}
	// Nothing new: no segment written.
	res, err := Build(r.dir, dir, BuildOptions{NoCompact: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.NewCommits != 0 || res.Segment != "" {
		t.Fatalf("rebuild indexed %d commits", res.NewCommits)
	}
	for i := range 3 {
		r.write("src/b.go", fmt.Sprintf("v%d\n", i))
		r.commit(fmt.Sprintf("delta %d", i))
		if res, err = Build(r.dir, dir, BuildOptions{NoCompact: true}); err != nil {
			t.Fatal(err)
		}
		if res.NewCommits != 1 {
			t.Fatalf("delta %d indexed %d commits", i, res.NewCommits)
		}
	}
	ix, _ := Open(dir)
	if n := len(ix.Manifest.Segments); n != 4 {
		t.Fatalf("%d segments, want 4", n)
	}
	before := hashesOf(ix, ix.Log("src/b.go", true, nil, 0))
	ix.Close()

	if err := Compact(dir); err != nil {
		t.Fatal(err)
	}
	ix, _ = Open(dir)
	if n := len(ix.Manifest.Segments); n != 1 {
		t.Fatalf("%d segments after compaction, want 1", n)
	}
	if after := hashesOf(ix, ix.Log("src/b.go", true, nil, 0)); !slices.Equal(before, after) {
		t.Errorf("compaction changed history:\n%v\n%v", before, after)
	}
	// The blobs sidecar is merged in change order: every surviving blob
	// hash still matches git after compaction.
	checked := 0
	for i := uint32(0); i < ix.NumCommits(); i++ {
		h := ix.Hash(i).String()
		for _, ch := range ix.Changes(i) {
			if ch.Kind == 'D' {
				continue
			}
			if !ch.HasBlob {
				t.Fatalf("blob missing after compaction at %s %s", h[:8], ch.Path)
			}
			if want := r.git("rev-parse", h+":"+ch.Path); ch.Blob.String() != want {
				t.Errorf("blob %s:%s = %s, want %s", h[:8], ch.Path, ch.Blob, want)
			}
			checked++
		}
	}
	if checked < 10 {
		t.Errorf("only %d blobs checked", checked)
	}
	ix.Close()

	// Force-push: rewind main. No new commits, only the overlay changes.
	old := r.git("rev-parse", "HEAD")
	r.git("reset", "-q", "--hard", "HEAD~2")
	res, err = Build(r.dir, dir, BuildOptions{NoCompact: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.NewCommits != 0 {
		t.Fatalf("force-push reindexed %d commits", res.NewCommits)
	}
	ix, _ = Open(dir)
	defer ix.Close()
	oldIdx, err := ix.LookupHash(old)
	if err != nil {
		t.Fatal("rewritten commit dropped from index")
	}
	head, _ := ix.RefID("HEAD")
	if ix.Reachable(oldIdx, head) {
		t.Error("rewound commit still reachable from HEAD")
	}
}

// Readers open and query continuously while a writer appends deltas and
// compacts, which deletes the files earlier readers mapped.
func TestReadersSurviveCompaction(t *testing.T) {
	r := history(t)
	dir := t.TempDir()
	if _, err := Build(r.dir, dir, BuildOptions{NoCompact: true}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var held []*Index // keep some readers open across compactions
			defer func() {
				for _, ix := range held {
					ix.Close()
				}
			}()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				ix, err := Open(dir)
				if err != nil {
					errs <- err
					return
				}
				if n := len(ix.Log("README.md", false, nil, 0)); n == 0 {
					errs <- fmt.Errorf("empty history from a fresh reader")
				}
				for _, h := range held {
					if n := len(h.Log("README.md", false, nil, 0)); n == 0 {
						errs <- fmt.Errorf("empty history from a held reader")
					}
				}
				if i%10 == 0 && len(held) < 5 {
					held = append(held, ix)
				} else {
					ix.Close()
				}
			}
		}()
	}
	for i := range 12 {
		r.write("README.md", fmt.Sprintf("v%d\n", i))
		r.commit(fmt.Sprintf("readme %d", i))
		if _, err := Build(r.dir, dir, BuildOptions{NoCompact: true}); err != nil {
			t.Fatal(err)
		}
		if i%3 == 2 {
			if err := Compact(dir); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
