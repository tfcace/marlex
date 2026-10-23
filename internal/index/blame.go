package index

import (
	"errors"
	"fmt"
	"sort"

	"github.com/tfcace/marlex/internal/fb"
)

var ErrNoHunks = errors.New("this index has no hunks; build it with `mx build --hunks` or fetch them with `mx pull --hunks`")

// BlameLine says which commit last changed one line, and where the line
// sat in that commit's version of the file.
type BlameLine struct {
	Commit uint32
	Path   string // the file's name at Commit (differs across renames)
	Line   uint32 // 1-based line number in Commit's version
	// Boundary is set when history ran out before the line's origin was
	// found (a shallow index, or a path whose creation was not indexed).
	// Commit is then the oldest commit examined.
	Boundary bool
}

// Blame attributes each of the nLines lines of path, as of commit at, to the
// commit that last changed it. It walks only hunk headers: the line's number
// is mapped back through each older first-parent diff of the path until a
// hunk claims it. This is `git blame --first-parent` without any objects.
func (ix *Index) Blame(path string, at uint32, nLines int) ([]BlameLine, error) {
	out := make([]BlameLine, nLines)
	if nLines == 0 {
		return out, nil
	}
	chain := ix.firstParentChain(at)
	type pending struct {
		orig int    // index into out
		cur  uint32 // line number at the commit being examined
	}
	open := make([]pending, nLines)
	for i := range open {
		open[i] = pending{i, uint32(i + 1)}
	}

	id, ok := ix.StringID(path)
	if !ok {
		return nil, errors.New("path is not in the index: " + path)
	}
	upper := at + 1 // only commits below this, along the chain
	oldest := at
	for len(open) > 0 {
		hist := ix.PathHistory(id)
		next, renamed := uint32(0), false
		for i := len(hist) - 1; i >= 0 && len(open) > 0; i-- {
			c := hist[i]
			if c >= upper || !chain.Has(c) {
				continue
			}
			oldest = c
			s, l := ix.commitSeg(c)
			j, kind, ok := changeFor(s, l, id)
			if !ok {
				continue // the path appears here only as a rename source
			}
			h, err := ix.hunksOf(s)
			if err != nil {
				return nil, err
			}
			if h == nil {
				return nil, ErrNoHunks
			}
			name := ix.String(id)
			if kind == 'A' {
				for _, p := range open {
					out[p.orig] = BlameLine{Commit: c, Path: name, Line: p.cur}
				}
				open = nil
				break
			}
			lo, hi := h.ChangeOffsets[j], h.ChangeOffsets[j+1]
			var offset int64
			k := lo
			kept := open[:0]
			for _, p := range open { // ascending by cur, and stays so
				for k < hi {
					ns, nl := h.NewStart[k], h.NewLen[k]
					if (nl > 0 && p.cur >= ns+nl) || (nl == 0 && p.cur > ns) {
						offset += int64(h.OldLen[k]) - int64(nl)
						k++
						continue
					}
					break
				}
				if k < hi && h.NewLen[k] > 0 && p.cur >= h.NewStart[k] {
					out[p.orig] = BlameLine{Commit: c, Path: name, Line: p.cur}
					continue
				}
				p.cur = uint32(int64(p.cur) + offset)
				kept = append(kept, p)
			}
			open = kept
			if kind == 'R' {
				if old, ok := s.OldPath(j); ok {
					next, renamed, upper = old, true, c
					break
				}
			}
		}
		if !renamed {
			break
		}
		id = next
	}
	for _, p := range open {
		out[p.orig] = BlameLine{Commit: oldest, Path: ix.String(id), Line: p.cur, Boundary: true}
	}
	return out, nil
}

// LineCount returns how many lines path has at commit at, from hunks alone:
// every line a file has was added by some hunk along its first-parent history,
// so the sum of (new_len - old_len) over that history is its length.
func (ix *Index) LineCount(path string, at uint32) (int, error) {
	chain := ix.firstParentChain(at)
	id, ok := ix.StringID(path)
	if !ok {
		return 0, errors.New("path is not in the index: " + path)
	}
	upper := at + 1
	var n int64
	for {
		hist := ix.PathHistory(id)
		next, renamed, created := uint32(0), false, false
		for i := len(hist) - 1; i >= 0; i-- {
			c := hist[i]
			if c >= upper || !chain.Has(c) {
				continue
			}
			s, l := ix.commitSeg(c)
			j, kind, ok := changeFor(s, l, id)
			if !ok {
				continue
			}
			h, err := ix.hunksOf(s)
			if err != nil {
				return 0, err
			}
			if h == nil {
				return 0, ErrNoHunks
			}
			for k := h.ChangeOffsets[j]; k < h.ChangeOffsets[j+1]; k++ {
				n += int64(h.NewLen[k]) - int64(h.OldLen[k])
			}
			if kind == 'A' {
				created = true
				break
			}
			if kind == 'R' {
				if old, ok := s.OldPath(j); ok {
					next, renamed, upper = old, true, c
					break
				}
			}
		}
		if created || !renamed {
			return int(n), nil
		}
		id = next
	}
}

// changeFor finds the change to pathID in commit l of s.
func changeFor(s *fb.Segment, l int, pathID uint32) (uint32, byte, bool) {
	lo, hi := s.ChangeOffsets[l], s.ChangeOffsets[l+1]
	paths := s.ChangePaths[lo:hi]
	i := sort.Search(len(paths), func(i int) bool { return paths[i] >= pathID })
	for ; i < len(paths) && paths[i] == pathID; i++ {
		j := lo + uint32(i)
		if s.ChangeKinds[j] != 'D' {
			return j, s.ChangeKinds[j], true
		}
	}
	return 0, 0, false
}

// firstParentChain marks at and its first-parent ancestors. First-parent
// diffs attribute a merged branch's lines to the merge, as
// `git blame --first-parent` does, so blame walks only this chain.
func (ix *Index) firstParentChain(at uint32) Bitset {
	b := NewBitset(int(at) + 1)
	for c := at; ; {
		b.Set(c)
		ps := ix.Parents(c)
		if len(ps) == 0 {
			return b
		}
		c = ps[0]
	}
}

// HasHunks reports whether blame can run on this index.
func (ix *Index) HasHunks() bool {
	if !ix.Manifest.Hunks {
		return false
	}
	for _, s := range ix.commitSegs {
		if ix.hunkSide[s] == nil {
			return false
		}
	}
	return true
}

// Previous is what a commit did to one line of a file: it created the
// file, added the line, or rewrote it. Lines are paired by position inside
// a change: the change's first new line rewrote its first old line, and so
// on; new lines past the end of the old run were added.
type Previous struct {
	Created bool
	Added   bool
	OldPath string // the file's name in the commit's parent
	OldLine uint32 // the replaced line, 1-based, in the parent's version
	// OldStart and OldLen are the whole run of lines the change replaced.
	OldStart, OldLen uint32
}

// Previous says what the change in commit to path did to line, a line
// number in commit's version that blame attributed to commit.
func (ix *Index) Previous(path string, commit uint32, line uint32) (Previous, error) {
	id, ok := ix.StringID(path)
	if !ok {
		return Previous{}, errors.New("path is not in the index: " + path)
	}
	s, l := ix.commitSeg(commit)
	j, kind, ok := changeFor(s, l, id)
	if !ok {
		return Previous{}, fmt.Errorf("%s does not change %s", ix.Hash(commit).String()[:10], path)
	}
	if kind == 'A' {
		return Previous{Created: true}, nil
	}
	h, err := ix.hunksOf(s)
	if err != nil {
		return Previous{}, err
	}
	if h == nil {
		return Previous{}, ErrNoHunks
	}
	p := Previous{OldPath: path}
	if kind == 'R' {
		if old, ok := s.OldPath(j); ok {
			p.OldPath = ix.String(old)
		}
	}
	for k := h.ChangeOffsets[j]; k < h.ChangeOffsets[j+1]; k++ {
		ns, nl := h.NewStart[k], h.NewLen[k]
		if nl == 0 || line < ns || line >= ns+nl {
			continue
		}
		p.OldStart, p.OldLen = h.OldStart[k], h.OldLen[k]
		if line-ns >= p.OldLen {
			p.Added = true
			return p, nil
		}
		p.OldLine = p.OldStart + line - ns
		return p, nil
	}
	return Previous{}, fmt.Errorf("%s does not change line %d of %s", ix.Hash(commit).String()[:10], line, path)
}
