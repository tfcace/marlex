package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/tfcace/marlex/internal/index"
	"github.com/tfcace/marlex/internal/rank"
)

type orderOutput struct {
	Base    string        `json:"base,omitempty"`
	Changed []string      `json:"changed"`
	Units   []rank.Ranked `json:"units"`
}

// cmdOrder ranks the repository's test units for the current change: the
// ones that history and the import graph say are most likely to fail first.
func cmdOrder(e env, args []string) error {
	fs := newFlags("order", "[--base REV | --files A,B] [-n N] [--json | --list]")
	base := fs.String("base", "", "diff against the merge base with this revision (default: origin/main, main, origin/master, master)")
	files := fs.String("files", "", "comma-separated changed files (repo-relative), instead of asking git")
	adapters := fs.String("adapters", "", "comma-separated: go, files (Jest, Vitest, pytest); default: detect")
	asJSON := fs.Bool("json", false, "print JSON")
	list := fs.Bool("list", false, "one unit per line, in order: for a test runner's command line")
	limit := fs.Int("n", 0, "show only the top N (text output)")
	if len(parse(fs, args)) != 0 {
		return usageErr(fs)
	}
	res, err := computeOrder(e, *base, *files, *adapters)
	if err != nil {
		return err
	}
	switch {
	case *asJSON:
		return printJSON(res)
	case *list:
		for _, u := range res.Units {
			fmt.Println(u.Unit)
		}
		return nil
	}
	fmt.Printf("%d changed file(s), %d test units\n\n", len(res.Changed), len(res.Units))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\tSCORE\tUNIT\tWHY")
	for i, u := range res.Units {
		if *limit > 0 && i >= *limit {
			break
		}
		why := strings.Join(u.Reasons, "; ")
		if why == "" {
			why = "-"
		}
		fmt.Fprintf(w, "%d\t%.2f\t%s\t%s\n", i+1, u.Score, u.Unit, why)
	}
	return w.Flush()
}

func computeOrder(e env, base, files, adapterNames string) (*orderOutput, error) {
	ix, err := e.open()
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	r, err := e.gitRepo()
	if err != nil {
		return nil, err
	}

	out := &orderOutput{}
	var exclude index.Scope
	if files != "" {
		out.Changed = strings.Split(files, ",")
	} else {
		var mb string
		out.Changed, mb, err = changedFiles(r.root, base, func() (string, bool) { return e.branchBase(ix, r) })
		if err != nil {
			return nil, err
		}
		out.Base = mb
		exclude = branchCommits(r.root, ix, mb)
	}

	var adapters []rank.Adapter
	if adapterNames != "" {
		if adapters, err = rank.Adapters(strings.Split(adapterNames, ",")); err != nil {
			return nil, err
		}
	} else {
		adapters = rank.DetectAdapters(r.root)
	}
	if len(adapters) == 0 {
		return nil, fmt.Errorf("found no Go modules or Jest/Vitest/pytest test files; pass --adapters")
	}
	out.Units, err = rank.Rank(ix, r.root, adapters, out.Changed, rank.Options{Exclude: exclude})
	if err == nil && len(out.Units) == 0 {
		err = fmt.Errorf("found no test units")
	}
	return out, err
}

func git(root string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", root, "-c", "core.quotePath=false"}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// changedFiles lists repo-relative files changed since the merge base,
// including uncommitted and untracked files.
func changedFiles(root, base string, fallback func() (string, bool)) ([]string, string, error) {
	cands := []string{base}
	if base == "" {
		cands = []string{"origin/main", "main", "origin/master", "master"}
	}
	mb := ""
	var found []string
	for _, c := range cands {
		if _, err := git(root, "rev-parse", "--verify", "-q", c+"^{commit}"); err != nil {
			continue
		}
		found = append(found, c)
		if out, err := git(root, "merge-base", c, "HEAD"); err == nil {
			mb = out
			break
		}
	}
	if mb == "" && base == "" && fallback != nil {
		if h, ok := fallback(); ok {
			mb = h
		}
	}
	// Ranking against the wrong base would rank nothing and look fine, so
	// a change mx cannot see is an error.
	const how = "pass --files with the changed files (in CI, from the pull request's file list), or --base REV"
	switch {
	case mb != "":
	case base != "":
		return nil, "", fmt.Errorf("cannot find a merge base with %s; %s", base, how)
	case len(found) > 0:
		return nil, "", fmt.Errorf("can't see what this change touches: %s shares no history with HEAD in this clone (a depth-1 checkout?); %s, or fetch the base", found[0], how)
	default:
		return nil, "", fmt.Errorf("can't see what this change touches: there is no main or master branch to compare with (a pull-request checkout?); %s", how)
	}
	seen := map[string]bool{}
	var files []string
	for _, args := range [][]string{
		{"diff", "--name-only", "--no-renames", mb},
		{"ls-files", "--others", "--exclude-standard", "--full-name"},
	} {
		out, err := git(root, args...)
		if err != nil {
			return nil, "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		for _, f := range strings.Split(out, "\n") {
			if f != "" && !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	return files, mb, nil
}

// branchBase finds, from the index, the published commit this branch's
// change is measured from, for clones without a main branch to diff against.
// After mx ff that is the newest published commit among HEAD's
// ancestors: with main merged into the branch, that is the main commit it
// merged, so the change is the branch's own, as a pull request shows it. In
// a pull-request checkout that ff has not run in, it is HEAD's indexed
// parent. Its tree must be local.
func (e env) branchBase(ix *index.Index, r *gitRepo) (string, bool) {
	var cands []uint32
	if head, ok := e.head(ix); ok {
		from := ix.Manifest.LocalFrom
		if from == 0 || head < from {
			return "", false // HEAD is published history: there is no branch to diff
		}
		anc := ix.Ancestors(head)
		for c := int64(from) - 1; c >= 0; c-- {
			if anc.Has(uint32(c)) {
				cands = append(cands, uint32(c))
				break
			}
		}
	} else if b, ok := e.base(ix); ok {
		cands = append(cands, b)
	}
	for _, c := range cands {
		h := ix.Hash(c).String()
		if _, err := git(r.root, "cat-file", "-e", h+"^{tree}"); err == nil {
			return h, true
		}
	}
	return "", false
}

// branchCommits returns the indexed commits in mb..HEAD, which must not
// count as evidence for their own co-change.
func branchCommits(root string, ix *index.Index, mb string) index.Scope {
	if mb == "" || mb == "HEAD" {
		return nil
	}
	out, err := git(root, "rev-list", mb+"..HEAD")
	if err != nil || out == "" {
		return nil
	}
	set := map[uint32]bool{}
	for _, h := range strings.Fields(out) {
		if idx, err := ix.LookupHash(h); err == nil {
			set[idx] = true
		}
	}
	return func(i uint32) bool { return set[i] }
}
