package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tfcace/marlex/internal/fb"
	"github.com/tfcace/marlex/internal/index"
	"github.com/tfcace/marlex/internal/sync"
)

func cmdBuild(e env, args []string) error {
	fs := newFlags("build", "[--hunks] [--refs PATTERNS] [--all-parents] [--no-compact]")
	hunks := fs.Bool("hunks", false, "also record line ranges, for blame (diffs file contents, so slower)")
	refs := fs.String("refs", "", "ref patterns tracked for --rev (default refs/heads,refs/remotes; remembered)")
	allParents := fs.Bool("all-parents", false, "diff merges against every parent, not just the first")
	noCompact := fs.Bool("no-compact", false, "never compact automatically")
	if len(parse(fs, args)) != 0 {
		return usageErr(fs)
	}
	dir, err := e.indexDir()
	if err != nil {
		return err
	}
	root := e.repo
	if r, err := e.gitRepo(); err == nil {
		root = r.root
	}
	j := newJob("mx/build", "indexing", "commits")
	opts := index.BuildOptions{AllParents: *allParents, NoCompact: *noCompact, Hunks: *hunks, Log: j, Progress: j.Progress}
	if *refs != "" {
		opts.RefPatterns = strings.Split(*refs, ",")
	}
	t0 := time.Now()
	res, err := index.Build(root, dir, opts)
	var summary string
	switch {
	case err != nil:
	case res.NewCommits == 0:
		summary = fmt.Sprintf("up to date (%d refs)", res.Refs)
	default:
		summary = fmt.Sprintf("indexed %d commits in %s", res.NewCommits, time.Since(t0).Round(time.Millisecond))
		if res.Compacted {
			summary += "; compacted the index into one segment"
		}
	}
	j.Finish(summary, err)
	if err != nil {
		return err
	}
	j.Print(summary)
	return nil
}

func cmdStats(e env, args []string) error {
	fs := newFlags("stats", "[--tables] [--json]")
	tables := fs.Bool("tables", false, "break the core's bytes down by column")
	asJSON := fs.Bool("json", false, "print JSON")
	if len(parse(fs, args)) != 0 {
		return usageErr(fs)
	}
	ix, err := e.open()
	if err != nil {
		return err
	}
	defer ix.Close()
	if *tables {
		return tableStats(ix)
	}
	m := ix.Manifest
	var core, blobs, hunks int64
	for _, s := range m.Segments {
		core += s.Bytes
		blobs += s.BlobBytes
		hunks += s.HunkBytes
	}
	core += m.ReachBytes
	refs := 0
	if ix.Reach != nil {
		refs = ix.Reach.Refs.Len()
	}
	if *asJSON {
		return printJSON(struct {
			index.Manifest
			Refs      int   `json:"refs"`
			CoreBytes int64 `json:"core_bytes"`
			BlobBytes int64 `json:"blob_bytes"`
			HunkBytes int64 `json:"hunk_bytes"`
			HasHunks  bool  `json:"has_hunks"`
		}{m, refs, core, blobs, hunks, ix.HasHunks()})
	}
	fmt.Printf("%d commits, %d paths and names, %d refs\n", m.NumCommits, m.NumStrings, refs)
	fmt.Printf("core %s in %d segment(s)", size(core), len(m.Segments))
	if blobs > 0 {
		fmt.Printf("; blob hashes %s", size(blobs))
	}
	if hunks > 0 {
		fmt.Printf("; hunks for blame %s", size(hunks))
	} else {
		fmt.Printf("; no hunks (build --hunks enables blame)")
	}
	fmt.Printf("\nupdated %s\n", m.Updated.Local().Format(time.RFC3339))
	return nil
}

// tableStats reports how many bytes each column takes across segments, to
// show where the index's size goes.
func tableStats(ix *index.Index) error {
	type col struct {
		name  string
		bytes int
	}
	var cols []col
	add := func(name string, n int) {
		for i := range cols {
			if cols[i].name == name {
				cols[i].bytes += n
				return
			}
		}
		cols = append(cols, col{name, n})
	}
	strBytes := func(s fb.Strings) int {
		n := 0
		for i := 0; i < s.Len(); i++ {
			n += 4 + 4 + len(s.Bytes(i)) + 1 // offset, length, data, NUL
		}
		return n
	}
	for _, s := range ix.Segs {
		add("commits: hashes", len(s.Hashes))
		add("commits: hash_order", 4*len(s.HashOrder))
		add("commits: authors+times", 4*len(s.Authors)+8*len(s.Times))
		add("commits: parents", 4*len(s.ParentOffsets)+4*len(s.Parents))
		add("commits: subjects", strBytes(s.Subjects))
		add("trees: change paths", 4*len(s.ChangeOffsets)+4*len(s.ChangePaths))
		add("trees: renames (sparse)", 4*len(s.RenameChanges)+4*len(s.RenameOldPaths))
		add("trees: kinds+mode codes", len(s.ChangeKinds)+len(s.ChangeModeCodes))
		add("path history", 4*len(s.PHPaths)+4*len(s.PHOffsets)+4*len(s.PHCommits))
		add("strings", strBytes(s.Strings)+8*len(s.StringHashes)+4*len(s.StringHashIDs))
	}
	all := 0
	for _, c := range cols {
		all += c.bytes
	}
	var blobs, hunks int64
	for _, si := range ix.Manifest.Segments {
		blobs += si.BlobBytes
		hunks += si.HunkBytes
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].bytes > cols[j].bytes })
	per := func(n int64) string { return fmt.Sprintf("%.0f B", float64(n)/float64(ix.NumCommits())) }
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COLUMN\tBYTES\tSHARE\tPER COMMIT")
	for _, c := range cols {
		fmt.Fprintf(w, "%s\t%s\t%.1f%%\t%s\n", c.name, size(int64(c.bytes)), 100*float64(c.bytes)/float64(all), per(int64(c.bytes)))
	}
	fmt.Fprintf(w, "core total\t%s\t\t%s\n", size(int64(all)), per(int64(all)))
	fmt.Fprintf(w, "optional: blob hashes\t%s\t\t%s\n", size(blobs), per(blobs))
	fmt.Fprintf(w, "optional: hunks\t%s\t\t%s\n", size(hunks), per(hunks))
	return w.Flush()
}

func cmdPublish(e env, args []string) error {
	fs := newFlags("publish", "DIR")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return usageErr(fs)
	}
	if r, err := e.gitRepo(); err == nil && r.shallow() {
		return errors.New("this index was pulled into a depth-1 clone; publish from the full clone where it was built")
	}
	dir, err := e.indexDir()
	if err != nil {
		return err
	}
	if ix, err := index.Open(dir); err == nil {
		local := ix.Manifest.LocalFrom
		ix.Close()
		if local > 0 {
			return errors.New("this index holds branch commits that mx ff added on top of a pulled index; publish from the full clone where it was built")
		}
	}
	j := newJob("mx/publish", "publishing", "files")
	st, err := sync.Publish(dir, pos[0])
	var summary string
	if err == nil {
		summary = fmt.Sprintf("published %d files (%s, compressed from %s); %d were already there", len(st.Copied), size(st.Bytes), size(st.RawBytes), st.Skipped)
	}
	j.Finish(summary, err)
	if err != nil {
		return err
	}
	j.Print(summary)
	return nil
}

func cmdPull(e env, args []string) error {
	fs := newFlags("pull", "DIR|URL [--hunks] [--blobs]")
	withHunks := fs.Bool("hunks", false, "also fetch the hunks that blame needs")
	withBlobs := fs.Bool("blobs", false, "also fetch blob hashes")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return usageErr(fs)
	}
	dir, err := e.indexDir()
	if err != nil {
		return err
	}
	t0 := time.Now()
	j := newJob("mx/pull", "pulling the index", "files")
	st, err := sync.Pull(pos[0], dir, sync.Sidecars{Blobs: *withBlobs, Hunks: *withHunks})
	var summary string
	if err == nil {
		summary = fmt.Sprintf("pulled %d files (%s transferred, %s on disk) in %s; %d were already here",
			len(st.Copied), size(st.Bytes), size(st.RawBytes), time.Since(t0).Round(time.Millisecond), st.Skipped)
	}
	j.Finish(summary, err)
	if err != nil {
		return err
	}
	j.Print(summary)
	return nil
}

// cmdFF brings a pulled index forward to this branch in a shallow clone. It
// deepens the clone until every commit where its history is cut off is
// already in the index (git alone can't stop there: --shallow-exclude stops
// at merges from main), then indexes the branch's commits on top. The
// result stays local: publish refuses it and the next pull replaces it.
func cmdFF(e env, args []string) error {
	fs := newFlags("ff", "[--remote NAME] [--max-rounds N]")
	remote := fs.String("remote", "origin", "remote to fetch history from")
	maxRounds := fs.Int("max-rounds", 12, "give up after this many fetches (each doubles the depth)")
	if len(parse(fs, args)) != 0 {
		return usageErr(fs)
	}
	r, err := e.gitRepo()
	if err != nil {
		return err
	}
	dir, err := e.indexDir()
	if err != nil {
		return err
	}
	if !r.shallow() {
		res, err := index.Build(r.root, dir, index.BuildOptions{})
		if err != nil {
			return err
		}
		fmt.Printf("not a shallow clone, so nothing to fetch; indexed %d new commit(s)\n", res.NewCommits)
		return nil
	}
	ix, err := openIndex(dir)
	if err != nil {
		return fmt.Errorf("%v; mx ff extends a pulled index", err)
	}
	head := e.headHash()
	if head == "" {
		ix.Close()
		return errors.New("HEAD has no commit yet")
	}
	t0 := time.Now()
	j := newJob("mx/ff", "fast-forwarding the index", "commits")
	fail := func(err error) error {
		j.Finish("", err)
		return err
	}
	rounds, depth := 0, 1
	for {
		boundary, err := index.ShallowBoundary(r.root)
		if err != nil {
			ix.Close()
			return fail(err)
		}
		outside := 0
		for h := range boundary {
			if _, err := ix.LookupHash(h); err != nil {
				outside++
			}
		}
		if outside == 0 {
			break
		}
		if rounds == *maxRounds {
			ix.Close()
			return fail(fmt.Errorf("after %d fetches this branch still reaches past the index (%d cut-off commit(s) outside it); "+
				"pull a newer index, or raise --max-rounds", rounds, outside))
		}
		fmt.Fprintf(j, "fetching %d more commit(s) of history (round %d)\n", depth, rounds+1)
		cmd := exec.Command("git", "-C", r.root, "fetch", "-q", "--deepen", strconv.Itoa(depth), *remote, head)
		if out, err := cmd.CombinedOutput(); err != nil {
			ix.Close()
			return fail(fmt.Errorf("git fetch --deepen %d %s %.10s: %v: %s", depth, *remote, head, err, strings.TrimSpace(string(out))))
		}
		rounds++
		depth *= 2
	}
	ix.Close()
	res, err := index.Build(r.root, dir, index.BuildOptions{Log: j, Progress: j.Progress})
	if err != nil {
		return fail(err)
	}
	summary := fmt.Sprintf("fetched history in %d round(s); indexed %d branch commit(s) on top of the pulled index in %s",
		rounds, res.NewCommits, time.Since(t0).Round(time.Millisecond))
	j.Finish(summary, nil)
	j.Print(summary)
	return nil
}
