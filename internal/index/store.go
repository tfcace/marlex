// Package index is the storage layer: a stack of immutable, memory-mapped
// FlatBuffers segments described by a small JSON manifest, plus a reachability
// overlay that is rebuilt on every update.
package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/tfcace/marlex/internal/fb"
)

const ManifestName = "manifest.json"

type SegmentInfo struct {
	File       string `json:"file"`
	Kind       string `json:"kind"` // base, delta, tests
	BaseCommit uint32 `json:"base_commit"`
	Commits    uint32 `json:"commits"`
	BaseString uint32 `json:"base_string"`
	Strings    uint32 `json:"strings"`
	Bytes      int64  `json:"bytes"`
	// Blobs names the optional sidecar with this segment's blob hashes.
	Blobs     string `json:"blobs,omitempty"`
	BlobBytes int64  `json:"blob_bytes,omitempty"`
	// Hunks names the optional sidecar with line ranges for blame.
	Hunks     string `json:"hunks,omitempty"`
	HunkBytes int64  `json:"hunk_bytes,omitempty"`
}

type Manifest struct {
	Version    int           `json:"version"`
	Segments   []SegmentInfo `json:"segments"`
	Reach      string        `json:"reach,omitempty"`
	ReachBytes int64         `json:"reach_bytes,omitempty"`
	NumCommits uint32        `json:"num_commits"`
	NumStrings uint32        `json:"num_strings"`
	// RefPatterns are the refs the reachability overlay tracks, kept so a
	// refresh rebuilds it the same way the last explicit build did.
	RefPatterns []string `json:"ref_patterns,omitempty"`
	// LocalFrom is set when a shallow clone indexed commits on top of a
	// pulled index: commits from this index up exist only in that clone.
	// Such an index is never published; the next pull replaces it.
	LocalFrom uint32 `json:"local_from,omitempty"`
	// Hunks is set when every commit segment was built with hunks.
	Hunks   bool      `json:"hunks,omitempty"`
	Updated time.Time `json:"updated"`
}

// Index is an open, read-only view of one manifest generation. Files it maps
// stay valid even if a writer swaps the manifest and deletes them.
type Index struct {
	Dir      string
	Manifest Manifest
	Segs     []*fb.Segment
	Reach    *fb.Reach

	commitSegs []*fb.Segment // segments with commits, by BaseCommit
	stringSegs []*fb.Segment // segments with strings, by BaseString
	maps       [][]byte

	// Optional sidecars this consumer has. They are opened with the index
	// but mapped on first use, so commands that never blame don't pay to map
	// the hunks. The open file keeps a sidecar readable if a compaction
	// deletes it in the meantime, as a mapping would.
	sideMu   sync.Mutex
	blobSide map[*fb.Segment]*os.File
	hunkSide map[*fb.Segment]*os.File
	blobs    map[*fb.Segment]*fb.Blobs // mapped and parsed on first use
	hunks    map[*fb.Segment]*fb.Hunks
}

// Open maps the current manifest generation. Writers swap the manifest before
// deleting the files it no longer names, so a file that vanishes while we
// open it means a newer manifest exists: start over with that one.
func Open(dir string) (*Index, error) {
	var err error
	for range 10 {
		var ix *Index
		ix, err = open(dir)
		if !errors.Is(err, os.ErrNotExist) {
			return ix, err
		}
	}
	return nil, err
}

func open(dir string) (*Index, error) {
	ix := &Index{Dir: dir, Manifest: Manifest{Version: fb.Version},
		blobSide: map[*fb.Segment]*os.File{}, hunkSide: map[*fb.Segment]*os.File{},
		blobs: map[*fb.Segment]*fb.Blobs{}, hunks: map[*fb.Segment]*fb.Hunks{}}
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return ix, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &ix.Manifest); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	for _, si := range ix.Manifest.Segments {
		buf, err := ix.mmap(si.File)
		if err != nil {
			ix.Close()
			return nil, err
		}
		seg, err := fb.ReadSegment(buf)
		if err != nil {
			ix.Close()
			return nil, fmt.Errorf("%s: %w", si.File, err)
		}
		ix.Segs = append(ix.Segs, seg)
		if seg.NumCommits() > 0 {
			ix.commitSegs = append(ix.commitSegs, seg)
		}
		if seg.Strings.Len() > 0 {
			ix.stringSegs = append(ix.stringSegs, seg)
		}
		for _, side := range []struct {
			name string
			into map[*fb.Segment]*os.File
		}{{si.Blobs, ix.blobSide}, {si.Hunks, ix.hunkSide}} {
			f, err := ix.openOptional(side.name)
			if err != nil {
				ix.Close()
				return nil, err
			}
			if f != nil {
				side.into[seg] = f
			}
		}
	}
	if ix.Manifest.Reach != "" {
		buf, err := ix.mmap(ix.Manifest.Reach)
		if err != nil {
			ix.Close()
			return nil, err
		}
		if ix.Reach, err = fb.ReadReach(buf); err != nil {
			ix.Close()
			return nil, err
		}
	}
	return ix, nil
}

func (ix *Index) mmap(name string) ([]byte, error) {
	f, err := os.Open(filepath.Join(ix.Dir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ix.mmapFile(f, name)
}

func (ix *Index) mmapFile(f *os.File, name string) ([]byte, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf("%s: empty file", name)
	}
	buf, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap %s: %w", name, err)
	}
	ix.maps = append(ix.maps, buf)
	return buf, nil
}

// openOptional opens a sidecar if this consumer has it; nil means it does
// not (never pulled, or removed by a compaction since the manifest was read).
func (ix *Index) openOptional(name string) (*os.File, error) {
	if name == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(ix.Dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return f, err
}

// blobsOf maps and parses a segment's blob sidecar on first use; nil means
// this consumer does not have it.
func (ix *Index) blobsOf(s *fb.Segment) (*fb.Blobs, error) {
	ix.sideMu.Lock()
	defer ix.sideMu.Unlock()
	if b, ok := ix.blobs[s]; ok {
		return b, nil
	}
	f := ix.blobSide[s]
	if f == nil {
		return nil, nil
	}
	buf, err := ix.mmapFile(f, f.Name())
	if err != nil {
		return nil, err
	}
	b, err := fb.ReadBlobs(buf)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(f.Name()), err)
	}
	ix.blobs[s] = b
	return b, nil
}

// hunksOf is blobsOf for the hunks sidecar.
func (ix *Index) hunksOf(s *fb.Segment) (*fb.Hunks, error) {
	ix.sideMu.Lock()
	defer ix.sideMu.Unlock()
	if h, ok := ix.hunks[s]; ok {
		return h, nil
	}
	f := ix.hunkSide[s]
	if f == nil {
		return nil, nil
	}
	buf, err := ix.mmapFile(f, f.Name())
	if err != nil {
		return nil, err
	}
	h, err := fb.ReadHunks(buf)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(f.Name()), err)
	}
	ix.hunks[s] = h
	return h, nil
}

func (ix *Index) Close() error {
	ix.sideMu.Lock()
	defer ix.sideMu.Unlock()
	for _, m := range ix.maps {
		syscall.Munmap(m)
	}
	ix.maps = nil
	for _, side := range []map[*fb.Segment]*os.File{ix.blobSide, ix.hunkSide} {
		for s, f := range side {
			f.Close()
			delete(side, s)
		}
	}
	clear(ix.blobs)
	clear(ix.hunks)
	return nil
}

func (ix *Index) NumCommits() uint32 { return ix.Manifest.NumCommits }
func (ix *Index) NumStrings() uint32 { return ix.Manifest.NumStrings }

// writeFile stores data under a content-addressed name and returns the name.
func writeFile(dir, prefix string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	name := fmt.Sprintf("%s-%s.fb", prefix, hex.EncodeToString(sum[:12]))
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err == nil {
		return name, nil
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	return name, os.Rename(tmp, p)
}

// writeManifest swaps the manifest atomically; open readers are unaffected.
func writeManifest(dir string, m *Manifest) error {
	m.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ManifestName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, ManifestName))
}

// lock takes the single-writer lock for dir.
func lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "write.lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("index is locked by another writer (remove %s if stale)", p)
		}
		return nil, err
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()
	return func() { os.Remove(p) }, nil
}

// ---- Commits ----

type Hash [20]byte

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

type Commit struct {
	Idx     uint32
	Hash    Hash
	Author  string
	Time    time.Time
	Parents []uint32
	Subject string
}

type Change struct {
	Path    string
	OldPath string
	Kind    byte
	Mode    uint32
	Blob    Hash
	HasBlob bool // false when the Blobs sidecar was not fetched
}

func (ix *Index) commitSeg(idx uint32) (*fb.Segment, int) {
	segs := ix.commitSegs
	i := sort.Search(len(segs), func(i int) bool { return segs[i].BaseCommit > idx }) - 1
	if i < 0 || int(idx-segs[i].BaseCommit) >= segs[i].NumCommits() {
		panic(fmt.Sprintf("commit index %d out of range", idx))
	}
	return segs[i], int(idx - segs[i].BaseCommit)
}

func (ix *Index) Commit(idx uint32) Commit {
	s, l := ix.commitSeg(idx)
	c := Commit{
		Idx:     idx,
		Author:  ix.String(s.Authors[l]),
		Time:    time.Unix(s.Times[l], 0),
		Parents: s.Parents[s.ParentOffsets[l]:s.ParentOffsets[l+1]],
		Subject: s.Subjects.At(l),
	}
	copy(c.Hash[:], s.Hashes[20*l:])
	return c
}

func (ix *Index) Hash(idx uint32) Hash {
	s, l := ix.commitSeg(idx)
	var h Hash
	copy(h[:], s.Hashes[20*l:])
	return h
}

func (ix *Index) Time(idx uint32) int64 {
	s, l := ix.commitSeg(idx)
	return s.Times[l]
}

func (ix *Index) AuthorID(idx uint32) uint32 {
	s, l := ix.commitSeg(idx)
	return s.Authors[l]
}

func (ix *Index) Parents(idx uint32) []uint32 {
	s, l := ix.commitSeg(idx)
	return s.Parents[s.ParentOffsets[l]:s.ParentOffsets[l+1]]
}

// ChangedPathIDs returns the path IDs a commit touched (new paths, and old
// paths for renames).
func (ix *Index) ChangedPathIDs(idx uint32, dst []uint32) []uint32 {
	s, l := ix.commitSeg(idx)
	lo, hi := s.ChangeOffsets[l], s.ChangeOffsets[l+1]
	dst = append(dst, s.ChangePaths[lo:hi]...)
	rc := s.RenameChanges
	for r := sort.Search(len(rc), func(r int) bool { return rc[r] >= lo }); r < len(rc) && rc[r] < hi; r++ {
		dst = append(dst, s.RenameOldPaths[r])
	}
	return dst
}

func (ix *Index) Changes(idx uint32) []Change {
	s, l := ix.commitSeg(idx)
	lo, hi := s.ChangeOffsets[l], s.ChangeOffsets[l+1]
	out := make([]Change, 0, hi-lo)
	for j := lo; j < hi; j++ {
		c := Change{Path: ix.String(s.ChangePaths[j]), Kind: s.ChangeKinds[j], Mode: fb.Mode(s.ChangeModeCodes[j])}
		if old, ok := s.OldPath(j); ok {
			c.OldPath = ix.String(old)
		}
		if b, _ := ix.blobsOf(s); b != nil {
			c.HasBlob = true
			copy(c.Blob[:], b.Hashes[20*j:])
		}
		out = append(out, c)
	}
	return out
}

// renameSource returns the old path ID if commit idx renamed something to pathID.
func (ix *Index) renameSource(idx, pathID uint32) (uint32, bool) {
	s, l := ix.commitSeg(idx)
	lo, hi := s.ChangeOffsets[l], s.ChangeOffsets[l+1]
	paths := s.ChangePaths[lo:hi]
	j := sort.Search(len(paths), func(j int) bool { return paths[j] >= pathID })
	for ; j < len(paths) && paths[j] == pathID; j++ {
		if s.ChangeKinds[int(lo)+j] == 'R' {
			return s.OldPath(lo + uint32(j))
		}
	}
	return 0, false
}

// LookupHash resolves a full or abbreviated hex hash.
func (ix *Index) LookupHash(prefix string) (uint32, error) {
	if len(prefix) < 4 || len(prefix) > 40 {
		return 0, fmt.Errorf("hash prefix %q: want 4 to 40 hex digits", prefix)
	}
	padded := prefix
	if len(padded)%2 == 1 {
		padded += "0"
	}
	want, err := hex.DecodeString(padded)
	if err != nil {
		return 0, fmt.Errorf("bad hash %q", prefix)
	}
	var found []uint32
	for _, s := range ix.commitSegs {
		order := s.HashOrder
		i := sort.Search(len(order), func(i int) bool {
			l := int(order[i])
			return bytes.Compare(s.Hashes[20*l:20*l+20], want) >= 0
		})
		for ; i < len(order); i++ {
			l := int(order[i])
			if hex.EncodeToString(s.Hashes[20*l : 20*l+20])[:len(prefix)] != prefix {
				break
			}
			found = append(found, s.BaseCommit+uint32(l))
		}
	}
	switch len(found) {
	case 0:
		return 0, fmt.Errorf("commit %s is not indexed", prefix)
	case 1:
		return found[0], nil
	}
	return 0, fmt.Errorf("hash prefix %s is ambiguous (%d commits)", prefix, len(found))
}

// ---- Strings ----

func strHash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

func (ix *Index) String(id uint32) string {
	segs := ix.stringSegs
	i := sort.Search(len(segs), func(i int) bool { return segs[i].BaseString > id }) - 1
	if i < 0 || int(id-segs[i].BaseString) >= segs[i].Strings.Len() {
		panic(fmt.Sprintf("string ID %d out of range", id))
	}
	return segs[i].Strings.At(int(id - segs[i].BaseString))
}

func (ix *Index) StringID(s string) (uint32, bool) {
	h := strHash(s)
	for _, seg := range ix.stringSegs {
		hs := seg.StringHashes
		i := sort.Search(len(hs), func(i int) bool { return hs[i] >= h })
		for ; i < len(hs) && hs[i] == h; i++ {
			id := seg.StringHashIDs[i]
			if string(seg.Strings.Bytes(int(id-seg.BaseString))) == s {
				return id, true
			}
		}
	}
	return 0, false
}

// ---- Path history ----

// PathHistory returns the ascending global indices of commits whose
// first-parent diff touched pathID.
func (ix *Index) PathHistory(pathID uint32) []uint32 {
	var parts [][]uint32
	total := 0
	for _, s := range ix.commitSegs {
		ps := s.PHPaths
		i := sort.Search(len(ps), func(i int) bool { return ps[i] >= pathID })
		if i < len(ps) && ps[i] == pathID {
			p := s.PHCommits[s.PHOffsets[i]:s.PHOffsets[i+1]]
			parts = append(parts, p)
			total += len(p)
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	out := make([]uint32, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ---- Reachability ----

// RefID returns the overlay column for a ref name ("HEAD", "main",
// "refs/heads/main", "origin/main").
func (ix *Index) RefID(name string) (int, bool) {
	if ix.Reach == nil {
		return 0, false
	}
	cands := []string{name, "refs/heads/" + name, "refs/remotes/" + name, "refs/tags/" + name}
	for _, c := range cands {
		for i := 0; i < ix.Reach.Refs.Len(); i++ {
			if ix.Reach.Refs.At(i) == c {
				return i, true
			}
		}
	}
	return 0, false
}

func (ix *Index) Reachable(idx uint32, ref int) bool {
	r := ix.Reach
	if r == nil || idx >= r.NumCommits {
		return false
	}
	w := r.Bitmap[int(idx)*int(r.WordsPerCommit)+ref/64]
	return w&(1<<(ref%64)) != 0
}

func (ix *Index) Children(idx uint32) []uint32 {
	r := ix.Reach
	if r == nil || idx >= r.NumCommits {
		return nil
	}
	return r.Children[r.ChildrenOffsets[idx]:r.ChildrenOffsets[idx+1]]
}

// Ancestors returns a bitset of every commit reachable from idx, including idx.
// Parents always have lower indices, so a single descending sweep suffices.
func (ix *Index) Ancestors(idx uint32) Bitset {
	b := NewBitset(int(idx) + 1)
	b.Set(idx)
	for i := int64(idx); i >= 0; i-- {
		if !b.Has(uint32(i)) {
			continue
		}
		for _, p := range ix.Parents(uint32(i)) {
			b.Set(p)
		}
	}
	return b
}

type Bitset []uint64

func NewBitset(n int) Bitset       { return make(Bitset, (n+63)/64) }
func (b Bitset) Set(i uint32)      { b[i/64] |= 1 << (i % 64) }
func (b Bitset) Has(i uint32) bool { return int(i/64) < len(b) && b[i/64]&(1<<(i%64)) != 0 }
