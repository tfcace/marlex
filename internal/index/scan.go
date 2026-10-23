package index

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/tfcace/marlex/internal/fb"
)

const commitSep = "\x1e"

// chunkSize is how many commits one git process reads. Small chunks keep
// memory flat: at most 2*workers chunks are parsed and waiting at once.
const chunkSize = 2000

type hunk struct{ os, ol, ns, nl uint32 }

type rawChange struct {
	kind      byte
	mode      uint32
	blob      []byte
	path, old string
	hunks     []hunk
}

type rawCommit struct {
	hash    string
	parents []string
	author  string
	time    int64
	subject string
	changes []rawChange
}

// scanCommits reads the new commits' metadata and first-parent changes
// (and, with hunks, their line ranges) and builds the segment columns. The
// commits are split into contiguous chunks, each read by its own `git log`
// in parallel; diffing is git's cost, so this scales with cores. Chunks are
// assembled in order, so string IDs and the output are the same as a single
// pass would produce.
func scanCommits(repo string, ix *Index, in *interner, hashes []string, allParents, withHunks bool, progress func(done, total int)) (*fb.SegmentData, []byte, *fb.HunksData, error) {
	workers := min(runtime.NumCPU(), 8)
	if n, err := strconv.Atoi(os.Getenv("MX_SCAN_WORKERS")); err == nil && n > 0 {
		workers = n
	}
	type result struct {
		recs []rawCommit
		err  error
	}
	var results []chan result
	for lo := 0; lo < len(hashes); lo += chunkSize {
		results = append(results, make(chan result, 1))
	}
	// slots bounds chunks that are running or parsed but not yet assembled.
	slots := make(chan struct{}, 2*workers)
	running := make(chan struct{}, workers)
	go func() {
		for i, ch := range results {
			lo := i * chunkSize
			chunk := hashes[lo:min(lo+chunkSize, len(hashes))]
			slots <- struct{}{}
			running <- struct{}{}
			go func() {
				recs, err := scanChunk(repo, chunk, allParents, withHunks)
				<-running
				ch <- result{recs, err}
			}()
		}
	}()

	base := ix.NumCommits()
	d := &fb.SegmentData{BaseCommit: base, BaseString: ix.NumStrings(), ParentOffsets: []uint32{0}, ChangeOffsets: []uint32{0}}
	var blobs []byte
	var hd *fb.HunksData
	if withHunks {
		hd = &fb.HunksData{ChangeOffsets: []uint32{0}}
	}
	local := make(map[string]uint32, len(hashes))
	for i, h := range hashes {
		local[h] = base + uint32(i)
	}
	resolve := func(h string) (uint32, bool) {
		if i, ok := local[h]; ok {
			return i, true
		}
		if ix.NumCommits() == 0 {
			return 0, false
		}
		i, err := ix.LookupHash(h)
		return i, err == nil
	}

	type change struct {
		path, old uint32
		raw       *rawChange
	}
	var cur []change
	var firstErr error
	for _, ch := range results {
		r := <-ch
		<-slots
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		if firstErr != nil {
			continue
		}
		for ci := range r.recs {
			c := &r.recs[ci]
			if want := hashes[len(d.Times)]; want != c.hash {
				return nil, nil, nil, fmt.Errorf("git log order mismatch: got %s, want %s", c.hash, want)
			}
			hb, err := hex.DecodeString(c.hash)
			if err != nil || len(hb) != 20 {
				return nil, nil, nil, fmt.Errorf("bad hash %q (only SHA-1 repositories are supported)", c.hash)
			}
			d.Hashes = append(d.Hashes, hb...)
			for _, p := range c.parents {
				if pi, ok := resolve(p); ok {
					d.Parents = append(d.Parents, pi)
				} // else: shallow boundary, parent unknown
			}
			d.ParentOffsets = append(d.ParentOffsets, uint32(len(d.Parents)))
			d.Authors = append(d.Authors, in.id(c.author))
			d.Times = append(d.Times, c.time)
			d.Subjects = append(d.Subjects, c.subject)

			cur = cur[:0]
			for k := range c.changes {
				rc := &c.changes[k]
				ch := change{old: fb.NoPath, raw: rc}
				if rc.old != "" {
					ch.old = in.id(rc.old)
				}
				ch.path = in.id(rc.path)
				cur = append(cur, ch)
			}
			sort.Slice(cur, func(a, b int) bool { return cur[a].path < cur[b].path })
			for _, ch := range cur {
				if ch.old != fb.NoPath {
					d.RenameChanges = append(d.RenameChanges, uint32(len(d.ChangePaths)))
					d.RenameOldPaths = append(d.RenameOldPaths, ch.old)
				}
				d.ChangePaths = append(d.ChangePaths, ch.path)
				d.ChangeKinds = append(d.ChangeKinds, ch.raw.kind)
				d.ChangeModeCodes = append(d.ChangeModeCodes, fb.ModeCode(ch.raw.mode))
				blobs = append(blobs, ch.raw.blob...)
				if hd != nil {
					for _, h := range ch.raw.hunks {
						hd.OldStart = append(hd.OldStart, h.os)
						hd.OldLen = append(hd.OldLen, h.ol)
						hd.NewStart = append(hd.NewStart, h.ns)
						hd.NewLen = append(hd.NewLen, h.nl)
					}
					hd.ChangeOffsets = append(hd.ChangeOffsets, uint32(len(hd.OldStart)))
				}
			}
			d.ChangeOffsets = append(d.ChangeOffsets, uint32(len(d.ChangePaths)))
		}
		if progress != nil {
			progress(len(d.Times), len(hashes))
		}
	}
	if firstErr != nil {
		return nil, nil, nil, firstErr
	}
	if len(d.Times) != len(hashes) {
		return nil, nil, nil, fmt.Errorf("git log returned %d commits, want %d", len(d.Times), len(hashes))
	}
	return d, blobs, hd, nil
}

// scanChunk runs one `git log --raw` (plus `-p -U0` for hunks) over hashes,
// in the given order, and parses it without interning anything.
func scanChunk(repo string, hashes []string, allParents, withHunks bool) ([]rawCommit, error) {
	diffMerges := "--diff-merges=first-parent"
	if allParents {
		diffMerges = "--diff-merges=separate"
	}
	args := []string{"-C", repo, "-c", "core.quotePath=false", "log", "--no-walk=unsorted", "--stdin",
		"--root", "-M", "--raw", "--no-abbrev", diffMerges,
		"--format=" + commitSep + "%H\x1f%P\x1f%an <%ae>\x1f%at\x1f%s"}
	if withHunks {
		// Only hunk headers matter; pin everything user config could change.
		args = append(args, "-p", "-U0", "--diff-algorithm=myers", "--src-prefix=a/", "--dst-prefix=b/",
			"--no-ext-diff", "--no-textconv", "--no-color")
	}
	cmd := exec.Command("git", args...)
	cmd.Stdin = strings.NewReader(strings.Join(hashes, "\n") + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	recs := make([]rawCommit, 0, len(hashes))
	var c *rawCommit
	seen := map[[2]string]bool{} // dedupe paths across parents in --diff-merges=separate
	byPath := map[string]int{}   // change index by path, for attaching hunks
	var inPatch, inHunk bool
	var minus, plus string
	addHunk := func(line string) {
		path := plus
		if path == "" {
			path = minus
		}
		k, ok := byPath[path]
		if !ok {
			return
		}
		if h, ok := parseHunk(line); ok {
			c.changes[k].hunks = append(c.changes[k].hunks, h)
		}
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, commitSep):
			f := strings.Split(line[len(commitSep):], "\x1f")
			if len(f) != 5 {
				return nil, fmt.Errorf("unexpected log header %q", line)
			}
			if c != nil && f[0] == c.hash {
				continue // --diff-merges=separate repeats the header per parent
			}
			t, _ := strconv.ParseInt(f[3], 10, 64)
			recs = append(recs, rawCommit{hash: f[0], parents: strings.Fields(f[1]), author: f[2], time: t, subject: f[4]})
			c = &recs[len(recs)-1]
			clear(seen)
			clear(byPath)
			inPatch, inHunk = false, false
		case c == nil:
		case inPatch:
			// Patch text (only with hunks). Changed lines start with + - or
			// \, so they never look like the headers matched here.
			switch {
			case strings.HasPrefix(line, "diff --git "):
				inHunk, minus, plus = false, "", ""
			case inHunk:
				if strings.HasPrefix(line, "@@ ") {
					addHunk(line)
				}
			case strings.HasPrefix(line, "--- "):
				minus = patchPath(line[4:])
			case strings.HasPrefix(line, "+++ "):
				plus = patchPath(line[4:])
			case strings.HasPrefix(line, "@@ "):
				inHunk = true
				addHunk(line)
			}
		case withHunks && strings.HasPrefix(line, "diff --git "):
			inPatch, inHunk, minus, plus = true, false, "", ""
		case strings.HasPrefix(line, ":"):
			// :100644 100644 <old> <new> M\tpath   or   R087\told\tnew
			tab := strings.IndexByte(line, '\t')
			if tab < 0 {
				continue
			}
			meta := strings.Fields(line[1:tab])
			paths := strings.Split(line[tab+1:], "\t")
			if len(meta) < 5 {
				continue
			}
			rc := rawChange{kind: meta[4][0]}
			mode, _ := strconv.ParseUint(meta[1], 8, 32)
			rc.mode = uint32(mode)
			rc.blob, _ = hex.DecodeString(meta[3])
			if len(rc.blob) != 20 {
				rc.blob = make([]byte, 20)
			}
			if (rc.kind == 'R' || rc.kind == 'C') && len(paths) == 2 {
				rc.old, rc.path = paths[0], paths[1]
			} else {
				rc.path = paths[0]
			}
			if k := [2]string{rc.path, rc.old}; !seen[k] {
				seen[k] = true
				byPath[rc.path] = len(c.changes)
				c.changes = append(c.changes, rc)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git log: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return recs, nil
}

// patchPath extracts the path from a "--- a/x" or "+++ b/x" header value.
func patchPath(v string) string {
	if v == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(v, "\"") {
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		}
	}
	if len(v) > 2 && (v[:2] == "a/" || v[:2] == "b/") {
		return v[2:]
	}
	return v
}

// parseHunk parses "@@ -os[,ol] +ns[,nl] @@".
func parseHunk(line string) (hunk, bool) {
	f := strings.Fields(line)
	if len(f) < 3 || len(f[1]) < 2 || len(f[2]) < 2 {
		return hunk{}, false
	}
	rng := func(s string) (uint32, uint32) {
		start, n, ok := strings.Cut(s[1:], ",")
		a, _ := strconv.ParseUint(start, 10, 32)
		b := uint64(1)
		if ok {
			b, _ = strconv.ParseUint(n, 10, 32)
		}
		return uint32(a), uint32(b)
	}
	var h hunk
	h.os, h.ol = rng(f[1])
	h.ns, h.nl = rng(f[2])
	return h, true
}
