package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tfcace/marlex/internal/index"
	"github.com/tfcace/marlex/internal/viz"
)

// Repositories up to this many commits get line ranges in their first
// build (prometheus, 21k commits: 5 s). Larger ones get the core index
// first (kubernetes, 163k: 3 s) and line ranges on the first line question.
const hunksWithFirstBuild = 60000

// firstBuild indexes a repository that has no index yet, so the first
// question answers instead of asking for a build. It shows the build as a
// meter on stderr, then a line on what was indexed.
func (e env) firstBuild(dir string) (*index.Index, error) {
	r, err := e.gitRepo()
	switch {
	case err != nil:
		return nil, errNoIndex
	case r.shallow():
		return nil, errors.New("this shallow clone has no index, and too little history to build one: mx pull URL brings a published index")
	case e.noRefresh:
		return nil, errors.New("no index here yet, and --no-refresh (or MX_NO_REFRESH) keeps mx from building one: mx build makes it")
	}
	n := 0
	if out, err := git(r.root, "rev-list", "--count", "--all"); err == nil {
		fmt.Sscan(out, &n)
	}
	name := filepath.Base(r.root)
	j := newJob("mx/build", "indexing "+name+" for the first time", "commits")
	if !j.tty {
		warnf("indexing %s for the first time (%s commits); later questions answer in milliseconds", name, comma(n))
	}
	t0 := time.Now()
	_, err = index.Build(r.root, dir, index.BuildOptions{Hunks: n <= hunksWithFirstBuild, Log: j.log(), Progress: j.Progress})
	if err != nil {
		j.Finish("", err)
		return nil, err
	}
	ix, err := openIndex(dir)
	if err != nil {
		j.Finish("", err)
		return nil, err
	}
	summary := fmt.Sprintf("indexed %s commits in %s%s", comma(int(ix.NumCommits())), time.Since(t0).Round(100*time.Millisecond), historyNote(ix))
	j.Finish(summary, nil)
	if !j.tty {
		warnf("%s", summary)
	}
	return ix, nil
}

// historyNote is ": 9 years of history by 312 people", the span and the
// number of authors the index holds.
func historyNote(ix *index.Index) string {
	n := ix.NumCommits()
	if n == 0 {
		return ""
	}
	authors := map[string]bool{}
	oldest, newest := time.Now(), time.Time{}
	for i := uint32(0); i < n; i++ {
		c := ix.Commit(i)
		authors[c.Author] = true
		if c.Time.Before(oldest) {
			oldest = c.Time
		}
		if c.Time.After(newest) {
			newest = c.Time
		}
	}
	span := newest.Sub(oldest)
	var age string
	switch years := span.Hours() / 24 / 365; {
	case years >= 1.5:
		age = fmt.Sprintf("%.0f years", years)
	case span >= 60*24*time.Hour:
		age = fmt.Sprintf("%.0f months", span.Hours()/24/30)
	default:
		age = plural(int(span.Hours()/24)+1, "day")
	}
	people := fmt.Sprintf("%s people", comma(len(authors)))
	if len(authors) == 1 {
		people = "1 person"
	}
	return fmt.Sprintf(": %s of history by %s", age, people)
}

// ensureHunks adds line ranges to an index that has none, the first time a
// question needs them, with a meter. Where it can't (a shallow clone, or
// --no-refresh), it returns the index as it is and the caller explains.
func (e env) ensureHunks(ix *index.Index) *index.Index {
	if ix.HasHunks() {
		return ix
	}
	r, err := e.gitRepo()
	if err != nil || r.shallow() || e.noRefresh {
		return ix
	}
	dir, err := e.indexDir()
	if err != nil {
		return ix
	}
	j := newJob("mx/build", "adding line ranges for blame (once)", "commits")
	if !j.tty {
		warnf("adding line ranges for blame to the index (once; later blames answer in milliseconds)")
	}
	t0 := time.Now()
	_, err = index.Build(r.root, dir, index.BuildOptions{Hunks: true, Log: j.log(), Progress: j.Progress})
	if err != nil {
		j.Finish("", err)
		return ix
	}
	j.Finish(fmt.Sprintf("added line ranges in %s", time.Since(t0).Round(100*time.Millisecond)), nil)
	fresh, err := openIndex(dir)
	if err != nil {
		return ix
	}
	ix.Close()
	return fresh
}

// greet is bare mx: what this repository has, and a few questions to
// try, built from its own busiest file so they can be pasted as they are.
func greet(e env) {
	color := colorOn()
	accent := func(s string) string { return viz.L(viz.S(s, viz.Accent)).String(color) }
	muted := func(s string) string { return viz.L(viz.S(s, viz.Muted)).String(color) }
	fmt.Println("mx remembers your repository's history, so you don't have to ask git.")
	fmt.Println()

	r, err := e.gitRepo()
	if err != nil {
		fmt.Println("This isn't inside a git repository. cd into one, or pass -C DIR.")
		fmt.Println()
		fmt.Println(muted("mx help lists every command"))
		return
	}
	name := filepath.Base(r.root)
	var ix *index.Index
	if dir, err := e.indexDir(); err == nil {
		ix, _ = openIndex(dir)
	}
	switch {
	case ix != nil:
		defer ix.Close()
		lines := "line ranges come with your first line question"
		if ix.HasHunks() {
			lines = "with line ranges for blame"
		}
		fmt.Printf("%s: %s commits indexed, %s.\n", name, comma(int(ix.NumCommits())), lines)
	case r.shallow():
		fmt.Printf("%s is a shallow clone without an index: mx pull URL brings a published one.\n", name)
	default:
		fmt.Printf("%s isn't indexed yet. Your first question indexes it, in seconds for most repositories.\n", name)
	}

	file, line := exampleFile(e, r, ix)
	dir := path.Dir(file)
	if dir == "." || file == "" {
		dir = "."
	}
	if file == "" {
		file = "FILE"
	}
	rows := [][2]string{
		{fmt.Sprintf("mx why %s:%d", file, line), "why this line is the way it is"},
		{"mx log --follow " + file, "the file's commits, across renames"},
		{"mx cochange " + file, "what usually changes with it"},
		{"mx owners " + dir, "who knows this code"},
	}
	if line == 0 {
		rows[0][0] = "mx why " + file
		rows[0][1] = "the file's story in one card"
	}
	w := 0
	for _, r := range rows {
		w = max(w, len(r[0]))
	}
	fmt.Println()
	fmt.Println("Try:")
	for _, r := range rows {
		fmt.Printf("  %s%s  %s\n", accent(r[0]), strings.Repeat(" ", w-len(r[0])), muted(r[1]))
	}
	fmt.Println()
	fmt.Println(muted("mx help         every command"))
	fmt.Println(muted("mx help why     one command, with examples"))
}

// exampleFile picks the repository's busiest source file and a line in it
// worth asking about, written relative to the current directory.
func exampleFile(e env, r *gitRepo, ix *index.Index) (string, int) {
	var cands []string
	if ix != nil {
		for _, h := range ix.Hotspots(index.PathMatch{Dir: true}, time.Now().AddDate(-1, 0, 0), nil, 30) {
			cands = append(cands, h.Path)
		}
	}
	if len(cands) == 0 {
		out, _ := git(r.root, "log", "-n", "200", "--name-only", "--format=")
		count := map[string]int{}
		for _, f := range strings.Split(out, "\n") {
			if f != "" {
				count[f]++
			}
		}
		for f := range count {
			cands = append(cands, f)
		}
		sort.Slice(cands, func(i, j int) bool {
			return count[cands[i]] > count[cands[j]] || count[cands[i]] == count[cands[j]] && cands[i] < cands[j]
		})
	}
	for _, f := range cands {
		if !sourceFile(f) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		return relToCwd(e, r, f), interestingLine(splitLines(data))
	}
	return "", 0
}

func sourceFile(f string) bool {
	switch strings.ToLower(path.Ext(f)) {
	case "", ".md", ".txt", ".sum", ".lock", ".json", ".yaml", ".yml", ".toml", ".mod", ".svg", ".png":
		return false
	}
	return !strings.Contains(f, "testdata/") && !strings.Contains(f, "vendor/")
}

// interestingLine is a line a third of the way into the file with some
// code on it, rather than a blank line or a brace.
func interestingLine(lines []string) int {
	for i := len(lines) / 3; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); len(t) >= 16 && !strings.HasPrefix(t, "//") && !strings.HasPrefix(t, "#") {
			return i + 1
		}
	}
	return 0
}

func relToCwd(e env, r *gitRepo, f string) string {
	cwd, err := filepath.Abs(e.repo)
	if err != nil {
		return f
	}
	if d, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = d
	}
	rel, err := filepath.Rel(cwd, filepath.Join(r.root, filepath.FromSlash(f)))
	if err != nil || strings.HasPrefix(rel, "..") {
		return f
	}
	return filepath.ToSlash(rel)
}
