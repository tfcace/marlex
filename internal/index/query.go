package index

import (
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Scope limits queries to commits reachable from a revision. A nil Scope
// admits every indexed commit.
type Scope func(idx uint32) bool

// ResolveScope interprets rev as "all", an overlay ref name, a hash, or (when
// repo is set) anything `git rev-parse` understands. Overlay refs and ref tips
// answer from the bitmap; other commits fall back to an ancestor sweep.
func (ix *Index) ResolveScope(repo, rev string) (Scope, error) {
	if rev == "" || rev == "all" {
		return nil, nil
	}
	if ref, ok := ix.RefID(rev); ok {
		return func(idx uint32) bool { return ix.Reachable(idx, ref) }, nil
	}
	idx, err := ix.ResolveCommit(repo, rev)
	if err != nil {
		return nil, err
	}
	if ix.Reach != nil {
		for r, tip := range ix.Reach.Tips {
			if tip == idx {
				return func(i uint32) bool { return ix.Reachable(i, r) }, nil
			}
		}
	}
	anc := ix.Ancestors(idx)
	return anc.Has, nil
}

// ResolveCommit maps a revision to a global commit index.
func (ix *Index) ResolveCommit(repo, rev string) (uint32, error) {
	if ref, ok := ix.RefID(rev); ok {
		return ix.Reach.Tips[ref], nil
	}
	if idx, err := ix.LookupHash(rev); err == nil {
		return idx, nil
	}
	if repo != "" {
		out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "-q", rev+"^{commit}").Output()
		if err == nil {
			return ix.LookupHash(strings.TrimSpace(string(out)))
		}
	}
	return 0, fmt.Errorf("cannot resolve %q in the index", rev)
}

type LogEntry struct {
	Idx  uint32
	Path string // the path's name at this commit (differs after a rename)
}

// Log returns the commits that touched path, newest first. With follow, the
// walk continues under the old name at each rename, like `git log --follow`.
func (ix *Index) Log(path string, follow bool, scope Scope, limit int) []LogEntry {
	var out []LogEntry
	cur := path
	upper := ^uint32(0) // only consider commits below this index
	seen := map[string]bool{}
	for !seen[cur] {
		seen[cur] = true
		id, ok := ix.StringID(cur)
		if !ok {
			break
		}
		hist := ix.PathHistory(id)
		next := ""
		for i := len(hist) - 1; i >= 0; i-- {
			c := hist[i]
			if c >= upper || (scope != nil && !scope(c)) {
				continue
			}
			out = append(out, LogEntry{Idx: c, Path: cur})
			if limit > 0 && len(out) >= limit {
				return out
			}
			if follow {
				if old, ok := ix.renameSource(c, id); ok {
					next, upper = ix.String(old), c
					break
				}
			}
		}
		if next == "" {
			break
		}
		cur = next
	}
	return out
}

// Since returns commits with author time at or after t, newest index first.
func (ix *Index) Since(t time.Time, scope Scope) []uint32 {
	var out []uint32
	ts := t.Unix()
	for i := int64(ix.NumCommits()) - 1; i >= 0; i-- {
		c := uint32(i)
		if ix.Time(c) >= ts && (scope == nil || scope(c)) {
			out = append(out, c)
		}
	}
	return out
}

type PathStat struct {
	Path    string
	Commits int
	Authors int
	Last    time.Time
}

// Hotspots ranks live paths under prefix by churn since t.
func (ix *Index) Hotspots(under PathMatch, since time.Time, scope Scope, n int) []PathStat {
	type acc struct {
		commits int
		authors map[uint32]bool
		last    int64
		deleted bool
	}
	stats := map[uint32]*acc{}
	ts := since.Unix()
	for _, s := range ix.commitSegs {
		for l := 0; l < s.NumCommits(); l++ {
			idx := s.BaseCommit + uint32(l)
			if s.Times[l] < ts || (scope != nil && !scope(idx)) {
				continue
			}
			for j := s.ChangeOffsets[l]; j < s.ChangeOffsets[l+1]; j++ {
				p := s.ChangePaths[j]
				a := stats[p]
				if a == nil {
					a = &acc{authors: map[uint32]bool{}}
					stats[p] = a
				}
				a.commits++
				a.authors[s.Authors[l]] = true
				if s.Times[l] >= a.last {
					a.last = s.Times[l]
					a.deleted = s.ChangeKinds[j] == 'D'
				}
			}
		}
	}
	var out []PathStat
	for p, a := range stats {
		if a.deleted {
			continue
		}
		name := ix.String(p)
		if !under.Matches(name) {
			continue
		}
		out = append(out, PathStat{Path: name, Commits: a.commits, Authors: len(a.authors), Last: time.Unix(a.last, 0)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Commits != out[j].Commits {
			return out[i].Commits > out[j].Commits
		}
		return out[i].Path < out[j].Path
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

type AuthorStat struct {
	Author  string
	Commits int
	Score   float64 // recency-weighted commits, half-life 180 days
	Last    time.Time
}

// Owners ranks authors of commits touching paths under prefix.
func (ix *Index) Owners(under PathMatch, scope Scope, n int) []AuthorStat {
	type acc struct {
		commits int
		score   float64
		last    int64
	}
	stats := map[uint32]*acc{}
	now := time.Now().Unix()
	const halfLife = 180 * 24 * 3600
	for _, s := range ix.commitSegs {
		for l := 0; l < s.NumCommits(); l++ {
			idx := s.BaseCommit + uint32(l)
			if scope != nil && !scope(idx) {
				continue
			}
			hit := false
			for j := s.ChangeOffsets[l]; j < s.ChangeOffsets[l+1] && !hit; j++ {
				hit = under.Matches(string(ix.stringBytes(s.ChangePaths[j])))
			}
			if !hit {
				continue
			}
			a := stats[s.Authors[l]]
			if a == nil {
				a = &acc{}
				stats[s.Authors[l]] = a
			}
			a.commits++
			age := float64(now-s.Times[l]) / halfLife
			a.score += math.Exp2(-age)
			a.last = max(a.last, s.Times[l])
		}
	}
	var out []AuthorStat
	for id, a := range stats {
		out = append(out, AuthorStat{Author: ix.String(id), Commits: a.commits, Score: a.score, Last: time.Unix(a.last, 0)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

func (ix *Index) stringBytes(id uint32) []byte {
	segs := ix.stringSegs
	i := sort.Search(len(segs), func(i int) bool { return segs[i].BaseString > id }) - 1
	return segs[i].Strings.Bytes(int(id - segs[i].BaseString))
}

// PathMatch selects a file, or everything under a directory ("" with Dir
// set is the whole repository).
type PathMatch struct {
	Path string
	Dir  bool
}

func (m PathMatch) Matches(p string) bool {
	switch {
	case !m.Dir:
		return p == m.Path
	case m.Path == "":
		return true
	}
	return strings.HasPrefix(p, m.Path+"/")
}

type CoChange struct {
	Path       string
	Count      int
	Confidence float64 // P(other changes | path changes)
}

// CoChanges lists paths that changed in the same commits as path.
func (ix *Index) CoChanges(path string, n int) []CoChange {
	id, ok := ix.StringID(path)
	if !ok {
		return nil
	}
	hist := ix.PathHistory(id)
	counts := map[uint32]int{}
	var buf []uint32
	for _, c := range hist {
		buf = ix.ChangedPathIDs(c, buf[:0])
		for _, p := range buf {
			if p != id {
				counts[p]++
			}
		}
	}
	var out []CoChange
	for p, k := range counts {
		out = append(out, CoChange{Path: ix.String(p), Count: k, Confidence: float64(k) / float64(len(hist))})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Path < out[j].Path
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// IntersectCount counts common elements of two ascending lists.
func IntersectCount(a, b []uint32) int {
	n, i, j := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			n++
			i++
			j++
		}
	}
	return n
}
