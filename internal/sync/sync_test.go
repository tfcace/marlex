package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tfcace/marlex/internal/index"
)

func builtIndex(t *testing.T) string {
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=A", "-c", "user.email=a@x", "commit", "-q", "--allow-empty", "-m", "one"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("a\nb\n"), 0o644)
	for _, args := range [][]string{
		{"add", "f.txt"},
		{"-c", "user.name=A", "-c", "user.email=a@x", "commit", "-q", "-m", "two"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	dir := t.TempDir()
	if _, err := index.Build(repo, dir, index.BuildOptions{Hunks: true}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func names(dir string) []string {
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestPublishPullRoundTrip(t *testing.T) {
	src := builtIndex(t)
	bucket := t.TempDir()
	st, err := Publish(src, bucket)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names(bucket) {
		if n != index.ManifestName && !strings.HasSuffix(n, ".fb.zst") {
			t.Errorf("bucket holds uncompressed %s", n)
		}
	}
	if st.Bytes == 0 || st.RawBytes == 0 {
		t.Fatalf("publish stats %+v", st)
	}

	// Core only by default.
	dst := t.TempDir()
	if _, err := Pull(bucket, dst, Sidecars{}); err != nil {
		t.Fatal(err)
	}
	for _, n := range names(dst) {
		if strings.HasPrefix(n, "blobs-") || strings.HasPrefix(n, "hunks-") {
			t.Errorf("core pull fetched sidecar %s", n)
		}
	}
	ix, err := index.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	if ix.NumCommits() != 2 || ix.HasHunks() {
		t.Errorf("pulled core: %d commits, hunks %v", ix.NumCommits(), ix.HasHunks())
	}
	ix.Close()

	// Asking for hunks adds just the sidecars.
	st, err = Pull(bucket, dst, Sidecars{Hunks: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Copied) != 1 || !strings.HasPrefix(st.Copied[0], "hunks-") {
		t.Errorf("second pull copied %v, want only the hunks sidecar", st.Copied)
	}
	ix, _ = index.Open(dst)
	if !ix.HasHunks() {
		t.Error("hunks missing after pull --hunks")
	}
	ix.Close()
}

func TestPullRejectsTamperedFile(t *testing.T) {
	src := builtIndex(t)
	bucket := t.TempDir()
	if _, err := Publish(src, bucket); err != nil {
		t.Fatal(err)
	}
	// Replace one segment's compressed body with another file's.
	var seg, other string
	for _, n := range names(bucket) {
		switch {
		case strings.HasPrefix(n, "seg-"):
			seg = n
		case strings.HasPrefix(n, "reach-"):
			other = n
		}
	}
	data, _ := os.ReadFile(filepath.Join(bucket, other))
	os.WriteFile(filepath.Join(bucket, seg), data, 0o644)
	if _, err := Pull(bucket, t.TempDir(), Sidecars{}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("pull of a tampered segment: err = %v", err)
	}
}

func TestPullFromUncompressedBucket(t *testing.T) {
	src := builtIndex(t) // an index directory doubles as a legacy raw bucket
	dst := t.TempDir()
	if _, err := Pull(src, dst, AllSidecars); err != nil {
		t.Fatal(err)
	}
	ix, err := index.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if ix.NumCommits() != 2 || !ix.HasHunks() {
		t.Errorf("raw pull: %d commits, hunks %v", ix.NumCommits(), ix.HasHunks())
	}
}
