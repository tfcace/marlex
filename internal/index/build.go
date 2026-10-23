package index

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tfcace/marlex/internal/fb"
)

type BuildOptions struct {
	// AllParents records merge diffs against every parent instead of the
	// first parent only.
	AllParents bool
	// RefPatterns selects the refs tracked by the reachability overlay.
	RefPatterns []string
	// IndexPatterns selects the refs whose history is indexed. It is wider
	// than RefPatterns: commits only reachable from tags are worth indexing,
	// but a bitmap column per tag is not.
	IndexPatterns []string
	// NoCompact disables automatic compaction.
	NoCompact bool
	// Hunks records line ranges for blame (an optional sidecar). It makes the
	// build diff file contents, so it is off unless asked for; once an index
	// has hunks, every later build keeps them.
	Hunks bool
	Log   io.Writer
	// Progress, when set, is called as commits are indexed, with how many
	// of the new commits are done.
	Progress func(done, total int)
}

var (
	DefaultRefPatterns   = []string{"refs/heads", "refs/remotes"}
	DefaultIndexPatterns = []string{"refs/heads", "refs/remotes", "refs/tags"}
)

type BuildResult struct {
	NewCommits int
	Segment    string
	Refs       int
	Compacted  bool
	ScanTime   time.Duration
	WriteTime  time.Duration
	ReachTime  time.Duration
}

func git(repo string, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", repo, "-c", "core.quotePath=false"}, args...)...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// interner assigns global string IDs, reusing IDs already in the index.
type interner struct {
	ix   *Index
	next uint32
	m    map[string]uint32
	list []string
}

func newInterner(ix *Index) *interner {
	return &interner{ix: ix, next: ix.NumStrings(), m: map[string]uint32{}}
}

func (in *interner) id(s string) uint32 {
	if id, ok := in.m[s]; ok {
		return id
	}
	id, ok := in.ix.StringID(s)
	if !ok {
		id = in.next
		in.next++
		in.list = append(in.list, s)
	}
	in.m[s] = id
	return id
}

// Build indexes every commit reachable from the tracked refs that is not in
// the index yet, as one delta segment, then rebuilds the reachability overlay.
func Build(repo, dir string, opts BuildOptions) (*BuildResult, error) {
	unlock, err := lock(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	boundary, err := shallowBoundary(repo)
	if err != nil {
		return nil, err
	}
	if boundary != nil {
		// A shallow clone can only add commits on top of a pulled index, and
		// it keeps its pulled files as they are for the next pull to reuse.
		opts.NoCompact = true
	}
	if opts.IndexPatterns == nil {
		opts.IndexPatterns = DefaultIndexPatterns
	}
	logf := func(format string, a ...any) {
		if opts.Log != nil {
			fmt.Fprintf(opts.Log, format, a...)
		}
	}

	ix, err := Open(dir)
	if errors.Is(err, fb.ErrVersion) {
		// Git is the source of truth: a breaking format change is a rebuild.
		logf("index format changed (%v); rebuilding\n", err)
		if err := wipe(dir); err != nil {
			return nil, err
		}
		ix, err = Open(dir)
	}
	if err == nil && opts.Hunks && !ix.Manifest.Hunks && ix.NumCommits() > 0 {
		// Blame needs hunks for all of history, not just new commits.
		logf("adding hunks; rebuilding\n")
		ix.Close()
		if err := wipe(dir); err != nil {
			return nil, err
		}
		ix, err = Open(dir)
	}
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	withHunks := opts.Hunks || ix.Manifest.Hunks
	if opts.RefPatterns == nil {
		opts.RefPatterns = ix.Manifest.RefPatterns
	}
	if opts.RefPatterns == nil {
		opts.RefPatterns = DefaultRefPatterns
	}

	res := &BuildResult{}
	t0 := time.Now()
	refs, err := listRefs(repo, opts.RefPatterns)
	if err != nil {
		return nil, err
	}
	indexRefs, err := listRefs(repo, opts.IndexPatterns)
	if err != nil {
		return nil, err
	}
	var tips []string
	for _, r := range indexRefs {
		tips = append(tips, r.hash)
	}
	var newHashes []string
	if len(tips) > 0 {
		out, err := git(repo, strings.NewReader(strings.Join(tips, "\n")+"\n"), "rev-list", "--topo-order", "--reverse", "--stdin")
		if err != nil {
			return nil, err
		}
		for _, h := range strings.Fields(string(out)) {
			if ix.NumCommits() > 0 {
				if _, err := ix.LookupHash(h); err == nil {
					continue
				}
			}
			newHashes = append(newHashes, h)
		}
	}
	if boundary != nil && len(newHashes) > 0 {
		if ix.NumCommits() == 0 {
			return nil, errors.New("this clone is shallow, so its history is incomplete; build the index in a full clone, " +
				"publish it, and `mx pull` it here")
		}
		for _, h := range newHashes {
			if boundary[h] {
				return nil, fmt.Errorf("%w: commit %.10s is where this clone's history is cut off, so its changes can't be computed here; "+
					"`mx ff` fetches just enough history to index it", ErrShallowBoundary, h)
			}
		}
	}
	res.NewCommits = len(newHashes)

	m := ix.Manifest
	if len(newHashes) > 0 {
		logf("indexing %d new commits\n", len(newHashes))
		if boundary != nil && m.LocalFrom == 0 {
			m.LocalFrom = ix.NumCommits()
		}
		in := newInterner(ix)
		if withHunks && opts.AllParents {
			return nil, fmt.Errorf("hunks are recorded against the first parent; drop --all-parents")
		}
		d, blobs, hunks, err := scanCommits(repo, ix, in, newHashes, opts.AllParents, withHunks, opts.Progress)
		if err != nil {
			return nil, err
		}
		d.Strings = in.list
		res.ScanTime = time.Since(t0)
		t1 := time.Now()
		finishSegment(d)
		data := fb.WriteSegment(d)
		name, err := writeFile(dir, "seg", data)
		if err != nil {
			return nil, err
		}
		blobData := fb.WriteBlobs(d.BaseCommit, blobs)
		blobName, err := writeFile(dir, "blobs", blobData)
		if err != nil {
			return nil, err
		}
		kind := "delta"
		if ix.NumCommits() == 0 {
			kind = "base"
		}
		m.Segments = append(m.Segments, SegmentInfo{
			File: name, Kind: kind, BaseCommit: d.BaseCommit, Commits: uint32(len(d.Times)),
			BaseString: d.BaseString, Strings: uint32(len(d.Strings)), Bytes: int64(len(data)),
			Blobs: blobName, BlobBytes: int64(len(blobData)),
		})
		if hunks != nil {
			hb := fb.WriteHunks(d.BaseCommit, hunks)
			si := &m.Segments[len(m.Segments)-1]
			if si.Hunks, err = writeFile(dir, "hunks", hb); err != nil {
				return nil, err
			}
			si.HunkBytes = int64(len(hb))
			m.Hunks = true
		}
		m.NumCommits += uint32(len(d.Times))
		m.NumStrings += uint32(len(d.Strings))
		res.Segment = name
		res.WriteTime = time.Since(t1)
		if err := writeManifest(dir, &m); err != nil {
			return nil, err
		}
		ix.Close()
		if ix, err = Open(dir); err != nil {
			return nil, err
		}
		defer ix.Close()
	}

	t2 := time.Now()
	if err := writeReach(dir, ix, refs, opts.RefPatterns); err != nil {
		return nil, err
	}
	res.Refs = len(refs)
	res.ReachTime = time.Since(t2)

	if !opts.NoCompact && needsCompaction(ix.Manifest) {
		logf("compacting %d segments\n", len(ix.Manifest.Segments))
		if err := compact(dir, ix); err != nil {
			return nil, err
		}
		res.Compacted = true
	}
	return res, nil
}

// ErrShallowBoundary means a new commit's parents are not in this shallow
// clone, so its first-parent diff can't be computed without fetching more.
var ErrShallowBoundary = errors.New("not enough history in this clone")

// ShallowBoundary returns the commits where a shallow clone's history is
// cut off, or nil when the clone is complete.
func ShallowBoundary(repo string) (map[string]bool, error) { return shallowBoundary(repo) }

func shallowBoundary(repo string) (map[string]bool, error) {
	out, err := git(repo, nil, "rev-parse", "--git-path", "shallow")
	if err != nil {
		return nil, err
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(repo, p)
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, h := range strings.Fields(string(data)) {
		set[h] = true
	}
	return set, nil
}

type ref struct{ name, hash string }

func listRefs(repo string, patterns []string) ([]ref, error) {
	// %(*objectname) peels annotated tags to the commit they point at.
	args := append([]string{"for-each-ref", "--format=%(objectname) %(objecttype) %(refname) %(*objectname) %(*objecttype)"}, patterns...)
	out, err := git(repo, nil, args...)
	if err != nil {
		return nil, err
	}
	var refs []ref
	if h, err := git(repo, nil, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
		refs = append(refs, ref{"HEAD", strings.TrimSpace(string(h))})
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) == 5 && f[1] == "tag" && f[4] == "commit" {
			f = []string{f[3], "commit", f[2]}
		}
		if len(f) != 3 || f[1] != "commit" || strings.HasSuffix(f[2], "/HEAD") {
			continue
		}
		refs = append(refs, ref{f[2], f[0]})
	}
	return refs, nil
}

// finishSegment derives the lookup structures: hash order, path history,
// string hashes, and the sort order of test runs.
func finishSegment(d *fb.SegmentData) {
	n := len(d.Times)
	if n > 0 {
		d.HashOrder = make([]uint32, n)
		for i := range d.HashOrder {
			d.HashOrder[i] = uint32(i)
		}
		sort.Slice(d.HashOrder, func(a, b int) bool {
			x, y := d.HashOrder[a], d.HashOrder[b]
			return bytes.Compare(d.Hashes[20*x:20*x+20], d.Hashes[20*y:20*y+20]) < 0
		})

		hist := map[uint32][]uint32{}
		add := func(p, idx uint32) {
			if h := hist[p]; len(h) == 0 || h[len(h)-1] != idx {
				hist[p] = append(h, idx)
			}
		}
		r := 0
		for l := 0; l < n; l++ {
			idx := d.BaseCommit + uint32(l)
			for j := d.ChangeOffsets[l]; j < d.ChangeOffsets[l+1]; j++ {
				add(d.ChangePaths[j], idx)
				for r < len(d.RenameChanges) && d.RenameChanges[r] < j {
					r++
				}
				if r < len(d.RenameChanges) && d.RenameChanges[r] == j {
					add(d.RenameOldPaths[r], idx) // the old name's history ends here
				}
			}
		}
		d.PHPaths = make([]uint32, 0, len(hist))
		for p := range hist {
			d.PHPaths = append(d.PHPaths, p)
		}
		sort.Slice(d.PHPaths, func(a, b int) bool { return d.PHPaths[a] < d.PHPaths[b] })
		d.PHOffsets = []uint32{0}
		d.PHCommits = d.PHCommits[:0]
		for _, p := range d.PHPaths {
			d.PHCommits = append(d.PHCommits, hist[p]...)
			d.PHOffsets = append(d.PHOffsets, uint32(len(d.PHCommits)))
		}
	}

	if len(d.Strings) > 0 {
		order := make([]int, len(d.Strings))
		hs := make([]uint64, len(d.Strings))
		for i, s := range d.Strings {
			order[i] = i
			hs[i] = strHash(s)
		}
		sort.Slice(order, func(a, b int) bool { return hs[order[a]] < hs[order[b]] })
		d.StringHashes = make([]uint64, len(order))
		d.StringHashIDs = make([]uint32, len(order))
		for i, o := range order {
			d.StringHashes[i] = hs[o]
			d.StringHashIDs[i] = d.BaseString + uint32(o)
		}
	}

}

func permute[T any](v []T, order []int) []T {
	out := make([]T, len(order))
	for i, o := range order {
		out[i] = v[o]
	}
	return out
}

// writeReach rebuilds the reachability overlay. This is all a force-push
// costs: segments are untouched.
func writeReach(dir string, ix *Index, refs []ref, patterns []string) error {
	n := ix.NumCommits()
	rd := &fb.ReachData{NumCommits: n, Refs: []string{}, Tips: []uint32{}}
	for _, r := range refs {
		if n == 0 {
			break
		}
		idx, err := ix.LookupHash(r.hash)
		if err != nil {
			continue
		}
		rd.Refs = append(rd.Refs, r.name)
		rd.Tips = append(rd.Tips, idx)
	}
	w := (len(rd.Refs) + 63) / 64
	if w == 0 {
		w = 1
	}
	rd.WordsPerCommit = uint32(w)
	rd.Bitmap = make([]uint64, int(n)*w)
	for r, tip := range rd.Tips {
		rd.Bitmap[int(tip)*w+r/64] |= 1 << (r % 64)
	}
	counts := make([]uint32, n+1)
	for i := int64(n) - 1; i >= 0; i-- {
		row := rd.Bitmap[int(i)*w : int(i)*w+w]
		for _, p := range ix.Parents(uint32(i)) {
			counts[p+1]++
			prow := rd.Bitmap[int(p)*w : int(p)*w+w]
			for k := range row {
				prow[k] |= row[k]
			}
		}
	}
	for i := 1; i <= int(n); i++ {
		counts[i] += counts[i-1]
	}
	rd.ChildrenOffsets = counts
	rd.Children = make([]uint32, counts[n])
	fill := append([]uint32(nil), counts[:n]...)
	for i := uint32(0); i < n; i++ {
		for _, p := range ix.Parents(i) {
			rd.Children[fill[p]] = i
			fill[p]++
		}
	}

	data := fb.WriteReach(rd)
	name, err := writeFile(dir, "reach", data)
	if err != nil {
		return err
	}
	m := ix.Manifest
	old := m.Reach
	if old == name && slices.Equal(m.RefPatterns, patterns) {
		return nil
	}
	m.Reach, m.ReachBytes, m.RefPatterns = name, int64(len(data)), patterns
	if err := writeManifest(dir, &m); err != nil {
		return err
	}
	ix.Manifest = m
	if old != "" && old != name {
		os.Remove(filepath.Join(dir, old))
	}
	return nil
}

func needsCompaction(m Manifest) bool {
	if len(m.Segments) < 2 {
		return false
	}
	var base, rest int64
	for i, s := range m.Segments {
		if i == 0 {
			base = s.Bytes
		} else {
			rest += s.Bytes
		}
	}
	return len(m.Segments)-1 > 10 || rest > base/2
}

// Compact rebuilds the whole stack into one base segment.
func Compact(dir string) error {
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	ix, err := Open(dir)
	if err != nil {
		return err
	}
	defer ix.Close()
	if len(ix.Manifest.Segments) < 2 {
		return nil
	}
	return compact(dir, ix)
}

// compact merges every segment into one. Global commit indices and string IDs
// are preserved, so references from other tools stay valid.
func compact(dir string, ix *Index) error {
	d := &fb.SegmentData{ParentOffsets: []uint32{0}, ChangeOffsets: []uint32{0}}
	// Blobs survive compaction only if every commit segment's sidecar is
	// present; a consumer that never pulled them compacts without.
	var blobs []byte
	haveBlobs, haveHunks := true, ix.Manifest.Hunks
	for _, s := range ix.commitSegs {
		haveBlobs = haveBlobs && ix.blobSide[s] != nil
		haveHunks = haveHunks && ix.hunkSide[s] != nil
	}
	hd := &fb.HunksData{ChangeOffsets: []uint32{0}}
	for _, s := range ix.Segs {
		chBase := uint32(len(d.ChangePaths))
		for _, rc := range s.RenameChanges {
			d.RenameChanges = append(d.RenameChanges, chBase+rc)
		}
		d.RenameOldPaths = append(d.RenameOldPaths, s.RenameOldPaths...)
		if haveBlobs && ix.blobSide[s] != nil {
			b, err := ix.blobsOf(s)
			if err != nil {
				return err
			}
			blobs = append(blobs, b.Hashes...)
		}
		if haveHunks && ix.hunkSide[s] != nil {
			h, err := ix.hunksOf(s)
			if err != nil {
				return err
			}
			base := uint32(len(hd.OldStart))
			for _, o := range h.ChangeOffsets[1:] {
				hd.ChangeOffsets = append(hd.ChangeOffsets, base+o)
			}
			hd.OldStart = append(hd.OldStart, h.OldStart...)
			hd.OldLen = append(hd.OldLen, h.OldLen...)
			hd.NewStart = append(hd.NewStart, h.NewStart...)
			hd.NewLen = append(hd.NewLen, h.NewLen...)
		}
		for l := 0; l < s.NumCommits(); l++ {
			d.Hashes = append(d.Hashes, s.Hashes[20*l:20*l+20]...)
			d.Authors = append(d.Authors, s.Authors[l])
			d.Times = append(d.Times, s.Times[l])
			d.Parents = append(d.Parents, s.Parents[s.ParentOffsets[l]:s.ParentOffsets[l+1]]...)
			d.ParentOffsets = append(d.ParentOffsets, uint32(len(d.Parents)))
			d.Subjects = append(d.Subjects, s.Subjects.At(l))
			lo, hi := s.ChangeOffsets[l], s.ChangeOffsets[l+1]
			d.ChangePaths = append(d.ChangePaths, s.ChangePaths[lo:hi]...)
			d.ChangeKinds = append(d.ChangeKinds, s.ChangeKinds[lo:hi]...)
			d.ChangeModeCodes = append(d.ChangeModeCodes, s.ChangeModeCodes[lo:hi]...)
			d.ChangeOffsets = append(d.ChangeOffsets, uint32(len(d.ChangePaths)))
		}
		for i := 0; i < s.Strings.Len(); i++ {
			d.Strings = append(d.Strings, s.Strings.At(i))
		}
	}
	if uint32(len(d.Times)) != ix.NumCommits() || uint32(len(d.Strings)) != ix.NumStrings() {
		return fmt.Errorf("compaction: segments are not contiguous")
	}
	finishSegment(d)
	data := fb.WriteSegment(d)
	name, err := writeFile(dir, "seg", data)
	if err != nil {
		return err
	}
	si := SegmentInfo{
		File: name, Kind: "base", Commits: uint32(len(d.Times)), Strings: uint32(len(d.Strings)),
		Bytes: int64(len(data)),
	}
	if haveBlobs && len(d.Times) > 0 {
		bd := fb.WriteBlobs(0, blobs)
		if si.Blobs, err = writeFile(dir, "blobs", bd); err != nil {
			return err
		}
		si.BlobBytes = int64(len(bd))
	}
	if haveHunks && len(d.Times) > 0 {
		hb := fb.WriteHunks(0, hd)
		if si.Hunks, err = writeFile(dir, "hunks", hb); err != nil {
			return err
		}
		si.HunkBytes = int64(len(hb))
	}
	m := ix.Manifest
	old := m.Segments
	m.Segments = []SegmentInfo{si}
	m.Hunks = si.Hunks != ""
	if err := writeManifest(dir, &m); err != nil {
		return err
	}
	// Readers that mapped the old segments keep them until they close.
	for _, s := range old {
		if s.File != name {
			os.Remove(filepath.Join(dir, s.File))
		}
		if s.Blobs != "" && s.Blobs != si.Blobs {
			os.Remove(filepath.Join(dir, s.Blobs))
		}
		if s.Hunks != "" && s.Hunks != si.Hunks {
			os.Remove(filepath.Join(dir, s.Hunks))
		}
	}
	return nil
}

// wipe removes every index file so the directory can be rebuilt.
func wipe(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".fb") || n == ManifestName {
			if err := os.Remove(filepath.Join(dir, n)); err != nil {
				return err
			}
		}
	}
	return nil
}
