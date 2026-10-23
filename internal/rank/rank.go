// Package rank orders tests so the ones most likely to fail run first. Every
// test still runs; ranking only changes the order.
package rank

import (
	"fmt"
	"math"
	"sort"

	"github.com/tfcace/marlex/internal/index"
)

// Weights are the signal weights: co-change affinity between the change and
// a unit's test files, then static import distance from the change.
var Weights = struct{ CoChange, Static float64 }{2, 1}

const (
	// Commits touching more files than this (vendoring, reformatting, bulk
	// renames) say nothing about affinity and are ignored for co-change.
	maxCommitFiles = 50
	// A single shared commit (often the repository's first) is not
	// evidence that two files belong together.
	minCoChange = 2
)

type Options struct {
	// Exclude drops commits from co-change counting, typically the commits of
	// the change being ranked, which would otherwise vouch for themselves.
	Exclude index.Scope
}

type Ranked struct {
	Unit     string   `json:"unit"`
	Adapter  string   `json:"adapter"`
	Module   string   `json:"module,omitempty"`
	Score    float64  `json:"score"`
	CoChange float64  `json:"cochange"` // 0..1
	Distance int      `json:"distance"` // -1 when unreachable
	Reasons  []string `json:"reasons"`
}

// Rank scores units from the adapters against the changed files.
func Rank(ix *index.Index, repo string, adapters []Adapter, changed []string, opts Options) ([]Ranked, error) {
	var out []Ranked
	for _, a := range adapters {
		units, err := a.Units(repo, changed)
		if err != nil {
			return nil, fmt.Errorf("%s adapter: %w", a.Name(), err)
		}
		rs := rankUnits(ix, units, changed, opts)
		for i := range rs {
			rs[i].Adapter = a.Name()
		}
		out = append(out, rs...)
	}
	sortRanked(out)
	return out, nil
}

func rankUnits(ix *index.Index, units []Unit, changed []string, opts Options) []Ranked {
	// Signal 1: co-change affinity between changed paths and test files.
	changedHist := make([][]uint32, 0, len(changed))
	changedNames := make([]string, 0, len(changed))
	keep := func(c uint32) bool {
		if opts.Exclude != nil && opts.Exclude(c) {
			return false
		}
		var buf [maxCommitFiles + 1]uint32
		return len(ix.ChangedPathIDs(c, buf[:0])) <= maxCommitFiles
	}
	keepCache := map[uint32]bool{}
	filter := func(h []uint32) []uint32 {
		out := make([]uint32, 0, len(h))
		for _, c := range h {
			k, ok := keepCache[c]
			if !ok {
				k = keep(c)
				keepCache[c] = k
			}
			if k {
				out = append(out, c)
			}
		}
		return out
	}
	if ix.NumCommits() > 0 {
		for _, p := range changed {
			if id, ok := ix.StringID(p); ok {
				if h := filter(ix.PathHistory(id)); len(h) > 0 {
					changedHist = append(changedHist, h)
					changedNames = append(changedNames, p)
				}
			}
		}
	}

	out := make([]Ranked, len(units))
	for i, u := range units {
		r := Ranked{Unit: u.Name, Distance: u.Distance, Module: u.Module}

		if len(changedHist) > 0 && len(u.TestFiles) > 0 {
			var th []uint32
			isTestFile := map[string]bool{}
			for _, f := range u.TestFiles {
				isTestFile[f] = true
				if id, ok := ix.StringID(f); ok {
					th = append(th, ix.PathHistory(id)...)
				}
			}
			th = filter(dedupe(th))
			best, bestPath, bestK, bestN := 0.0, "", 0, 0
			for j, h := range changedHist {
				if isTestFile[changedNames[j]] {
					continue
				}
				k := index.IntersectCount(h, th)
				if k < minCoChange {
					continue
				}
				// +1 smoothing: two shared commits out of two is still thin.
				if c := float64(k) / float64(len(h)+1); c > best {
					best, bestPath, bestK, bestN = c, changedNames[j], k, len(h)
				}
			}
			r.CoChange = best
			if best > 0 {
				r.Reasons = append(r.Reasons, fmt.Sprintf("co-changed with %s in %d of %d commits", bestPath, bestK, bestN))
			}
		}

		// Signal 2: static distance, from the unit's tests to a changed file.
		static := 0.0
		if u.Distance >= 0 {
			static = 1 / float64(1+u.Distance)
			switch u.Distance {
			case 0:
				r.Reasons = append(r.Reasons, "changed directly")
			default:
				r.Reasons = append(r.Reasons, fmt.Sprintf("depends on changed code (distance %d)", u.Distance))
			}
		}
		r.Score = Weights.CoChange*r.CoChange + Weights.Static*static
		out[i] = r
	}
	return out
}

func sortRanked(rs []Ranked) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if d := a.Score - b.Score; math.Abs(d) > 1e-9 {
			return d > 0
		}
		return a.Unit < b.Unit
	})
}

func dedupe(v []uint32) []uint32 {
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	out := v[:0]
	for i, x := range v {
		if i == 0 || x != v[i-1] {
			out = append(out, x)
		}
	}
	return out
}
