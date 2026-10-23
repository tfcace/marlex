package index

import (
	"bytes"
	"path"
	"slices"
	"strings"
)

// HasHistory reports whether path, as a file, was ever touched.
func (ix *Index) HasHistory(p string) bool {
	id, ok := ix.StringID(p)
	return ok && len(ix.PathHistory(id)) > 0
}

// HasPathsUnder reports whether any file under dir was ever touched.
func (ix *Index) HasPathsUnder(dir string) bool {
	found := false
	ix.eachPathUnder(dir, func(uint32) bool { found = true; return false })
	return found
}

// PathsUnder returns the IDs of every file under dir with history ("" for
// the whole repository).
func (ix *Index) PathsUnder(dir string) []uint32 {
	var out []uint32
	ix.eachPathUnder(dir, func(id uint32) bool { out = append(out, id); return true })
	return out
}

// eachPathUnder scans the interned strings; paths are not sorted in the
// format yet, so this is linear in the number of strings (milliseconds at
// kubernetes scale).
func (ix *Index) eachPathUnder(dir string, fn func(id uint32) bool) {
	prefix := []byte(dir + "/")
	if dir == "" {
		prefix = nil
	}
	for _, s := range ix.stringSegs {
		for i := 0; i < s.Strings.Len(); i++ {
			if !bytes.HasPrefix(s.Strings.Bytes(i), prefix) {
				continue
			}
			id := s.BaseString + uint32(i)
			if len(ix.PathHistory(id)) > 0 && !fn(id) {
				return
			}
		}
	}
}

// LogDir returns the commits that touched any file under dir, newest first,
// like `git log -- dir`.
func (ix *Index) LogDir(dir string, scope Scope, limit int) []LogEntry {
	seen := map[uint32]bool{}
	var all []uint32
	for _, id := range ix.PathsUnder(dir) {
		for _, c := range ix.PathHistory(id) {
			if !seen[c] && (scope == nil || scope(c)) {
				seen[c] = true
				all = append(all, c)
			}
		}
	}
	slices.Sort(all)
	slices.Reverse(all)
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	out := make([]LogEntry, len(all))
	for i, c := range all {
		out[i] = LogEntry{Idx: c, Path: dir}
	}
	return out
}

// Suggest finds the path or directory with history closest to p, for
// "did you mean" messages. Candidates share p's parent directory or its last
// element, which catches a typo in either without comparing p against every
// path in a large repository.
func (ix *Index) Suggest(p string) (string, bool) {
	qdir, qbase := path.Split(p)
	qdir = strings.TrimSuffix(qdir, "/")
	best, bestD := "", 1<<30
	seen := map[string]bool{}
	consider := func(c string) {
		if seen[c] {
			return
		}
		seen[c] = true
		if d := editDistance(p, c); d < bestD || d == bestD && c < best {
			best, bestD = c, d
		}
	}
	ix.eachPathUnder("", func(id uint32) bool {
		for a := ix.String(id); a != "." && a != ""; a = path.Dir(a) {
			d, b := path.Split(a)
			if strings.TrimSuffix(d, "/") == qdir || b == qbase {
				consider(a)
			}
		}
		return true
	})
	limit := max(2, len(p)/4)
	return best, best != "" && bestD <= limit
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
