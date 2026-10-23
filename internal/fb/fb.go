// Package fb reads and writes the FlatBuffers tables described in
// schema/index.fbs.
//
// Readers never copy: numeric vectors are returned as Go slices that alias the
// underlying (usually memory-mapped) buffer. FlatBuffers aligns every vector to
// its element size and stores little-endian data, so on little-endian hosts a
// vector of uint32 is a valid []uint32.
package fb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"

	flatbuffers "github.com/google/flatbuffers/go"
)

// Version is the format generation. It changes only for breaking changes;
// readers refuse other generations (ErrVersion) and the index is rebuilt.
const Version = 2

var ErrVersion = errors.New("unsupported index format version")

// NoPath marks a change without an old path (anything but a rename or copy).
const NoPath = ^uint32(0)

func init() {
	var x uint16 = 1
	if *(*byte)(unsafe.Pointer(&x)) != 1 {
		panic("mx: big-endian hosts are not supported")
	}
}

func slot(field int) flatbuffers.VOffsetT { return flatbuffers.VOffsetT(4 + 2*field) }

type table struct{ t flatbuffers.Table }

func rootTable(buf []byte) (table, error) {
	if len(buf) < 8 {
		return table{}, fmt.Errorf("buffer too small (%d bytes)", len(buf))
	}
	n := flatbuffers.GetUOffsetT(buf)
	if int(n) >= len(buf) {
		return table{}, fmt.Errorf("corrupt root offset")
	}
	return table{flatbuffers.Table{Bytes: buf, Pos: n}}, nil
}

func (t table) u32(field int) uint32 { return t.t.GetUint32Slot(slot(field), 0) }

// vec returns the raw bytes of a vector field and its element count.
func (t table) vec(field int, elemSize int) ([]byte, int) {
	o := flatbuffers.UOffsetT(t.t.Offset(slot(field)))
	if o == 0 {
		return nil, 0
	}
	start := t.t.Vector(o)
	n := t.t.VectorLen(o)
	return t.t.Bytes[start : start+flatbuffers.UOffsetT(n*elemSize)], n
}

func (t table) bytes(field int) []byte {
	b, _ := t.vec(field, 1)
	return b
}

func (t table) u32s(field int) []uint32 {
	b, n := t.vec(field, 4)
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(&b[0])), n)
}

func (t table) u64s(field int) []uint64 {
	b, n := t.vec(field, 8)
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&b[0])), n)
}

func (t table) i64s(field int) []int64 {
	b, n := t.vec(field, 8)
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*int64)(unsafe.Pointer(&b[0])), n)
}

// strs is a lazily decoded vector of strings.
type Strings struct {
	t     *flatbuffers.Table
	start flatbuffers.UOffsetT
	n     int
}

func (t table) strs(field int) Strings {
	o := flatbuffers.UOffsetT(t.t.Offset(slot(field)))
	if o == 0 {
		return Strings{}
	}
	return Strings{t: &t.t, start: t.t.Vector(o), n: t.t.VectorLen(o)}
}

func (s Strings) Len() int { return s.n }

// At returns string i. The result aliases the buffer only for the duration of
// the call to String, which copies; callers on the hot path should use Bytes.
func (s Strings) At(i int) string { return string(s.Bytes(i)) }

func (s Strings) Bytes(i int) []byte {
	p := s.start + flatbuffers.UOffsetT(i*4)
	p += flatbuffers.GetUOffsetT(s.t.Bytes[p:])
	n := flatbuffers.UOffsetT(binary.LittleEndian.Uint32(s.t.Bytes[p:]))
	return s.t.Bytes[p+4 : p+4+n]
}

// Segment field numbers, matching schema/index.fbs.
const (
	segVersion = iota
	segBaseCommit
	segBaseString
	segHashes
	segHashOrder
	segAuthors
	segTimes
	segParentOffsets
	segParents
	segSubjects
	segChangeOffsets
	segChangePaths
	segChangeOldPathsV1 // deprecated in v2: sparse renames below
	segChangeKinds
	segChangeModesV1 // deprecated in v2: segChangeModeCodes
	segChangeBlobsV1 // deprecated in v2: Blobs sidecar
	segPHPaths
	segPHOffsets
	segPHCommits
	segStrings
	segStringHashes
	segStringHashIDs
	segTRTests
	segTRUnits
	segTRCommits
	segTRTimes
	segTRStatus
	segTRDurations
	segTRJobs
	segRenameChanges
	segRenameOldPaths
	segChangeModeCodes
	segNumFields
)

// Segment is a zero-copy view of one segment file.
type Segment struct {
	BaseCommit uint32
	BaseString uint32

	Hashes        []byte
	HashOrder     []uint32
	Authors       []uint32
	Times         []int64
	ParentOffsets []uint32
	Parents       []uint32
	Subjects      Strings

	ChangeOffsets   []uint32
	ChangePaths     []uint32
	ChangeKinds     []byte
	ChangeModeCodes []byte
	// Renames and copies are rare, so they are stored sparsely: the change
	// indices that have an old path, ascending, and those old paths.
	RenameChanges  []uint32
	RenameOldPaths []uint32

	PHPaths   []uint32
	PHOffsets []uint32
	PHCommits []uint32

	Strings       Strings
	StringHashes  []uint64
	StringHashIDs []uint32

	TRTests     []uint32
	TRUnits     []uint32
	TRCommits   []uint32
	TRTimes     []int64
	TRStatus    []byte
	TRDurations []uint32
	TRJobs      []uint32
}

func ReadSegment(buf []byte) (*Segment, error) {
	t, err := rootTable(buf)
	if err != nil {
		return nil, err
	}
	if v := t.u32(segVersion); v != Version {
		return nil, fmt.Errorf("%w: segment is v%d, this build reads v%d", ErrVersion, v, Version)
	}
	s := &Segment{
		BaseCommit:      t.u32(segBaseCommit),
		BaseString:      t.u32(segBaseString),
		Hashes:          t.bytes(segHashes),
		HashOrder:       t.u32s(segHashOrder),
		Authors:         t.u32s(segAuthors),
		Times:           t.i64s(segTimes),
		ParentOffsets:   t.u32s(segParentOffsets),
		Parents:         t.u32s(segParents),
		Subjects:        t.strs(segSubjects),
		ChangeOffsets:   t.u32s(segChangeOffsets),
		ChangePaths:     t.u32s(segChangePaths),
		ChangeKinds:     t.bytes(segChangeKinds),
		ChangeModeCodes: t.bytes(segChangeModeCodes),
		RenameChanges:   t.u32s(segRenameChanges),
		RenameOldPaths:  t.u32s(segRenameOldPaths),
		PHPaths:         t.u32s(segPHPaths),
		PHOffsets:       t.u32s(segPHOffsets),
		PHCommits:       t.u32s(segPHCommits),
		Strings:         t.strs(segStrings),
		StringHashes:    t.u64s(segStringHashes),
		StringHashIDs:   t.u32s(segStringHashIDs),
		TRTests:         t.u32s(segTRTests),
		TRUnits:         t.u32s(segTRUnits),
		TRCommits:       t.u32s(segTRCommits),
		TRTimes:         t.i64s(segTRTimes),
		TRStatus:        t.bytes(segTRStatus),
		TRDurations:     t.u32s(segTRDurations),
		TRJobs:          t.u32s(segTRJobs),
	}
	if n := len(s.Times); len(s.Hashes) != 20*n || len(s.ParentOffsets) != n+1 && n > 0 {
		return nil, fmt.Errorf("corrupt segment: %d commits, %d hash bytes", n, len(s.Hashes))
	}
	return s, nil
}

func (s *Segment) NumCommits() int { return len(s.Times) }

// SegmentData is the in-memory form a Segment is written from.
type SegmentData struct {
	BaseCommit uint32
	BaseString uint32

	Hashes        []byte
	Authors       []uint32
	Times         []int64
	ParentOffsets []uint32
	Parents       []uint32
	Subjects      []string

	ChangeOffsets   []uint32
	ChangePaths     []uint32
	ChangeKinds     []byte
	ChangeModeCodes []byte
	RenameChanges   []uint32
	RenameOldPaths  []uint32

	// Derived when empty: HashOrder from Hashes, path history from changes,
	// string hashes from Strings.
	HashOrder     []uint32
	PHPaths       []uint32
	PHOffsets     []uint32
	PHCommits     []uint32
	Strings       []string
	StringHashes  []uint64
	StringHashIDs []uint32

	TRTests     []uint32
	TRUnits     []uint32
	TRCommits   []uint32
	TRTimes     []int64
	TRStatus    []byte
	TRDurations []uint32
	TRJobs      []uint32
}

func u32Vec(b *flatbuffers.Builder, v []uint32) flatbuffers.UOffsetT {
	if v == nil {
		return 0
	}
	b.StartVector(4, len(v), 4)
	for i := len(v) - 1; i >= 0; i-- {
		b.PrependUint32(v[i])
	}
	return b.EndVector(len(v))
}

func u64Vec(b *flatbuffers.Builder, v []uint64) flatbuffers.UOffsetT {
	if v == nil {
		return 0
	}
	b.StartVector(8, len(v), 8)
	for i := len(v) - 1; i >= 0; i-- {
		b.PrependUint64(v[i])
	}
	return b.EndVector(len(v))
}

func i64Vec(b *flatbuffers.Builder, v []int64) flatbuffers.UOffsetT {
	if v == nil {
		return 0
	}
	b.StartVector(8, len(v), 8)
	for i := len(v) - 1; i >= 0; i-- {
		b.PrependInt64(v[i])
	}
	return b.EndVector(len(v))
}

func byteVec(b *flatbuffers.Builder, v []byte) flatbuffers.UOffsetT {
	if v == nil {
		return 0
	}
	return b.CreateByteVector(v)
}

func strVec(b *flatbuffers.Builder, v []string) flatbuffers.UOffsetT {
	if v == nil {
		return 0
	}
	offs := make([]flatbuffers.UOffsetT, len(v))
	for i, s := range v {
		offs[i] = b.CreateString(s)
	}
	b.StartVector(4, len(v), 4)
	for i := len(offs) - 1; i >= 0; i-- {
		b.PrependUOffsetT(offs[i])
	}
	return b.EndVector(len(v))
}

func addOffset(b *flatbuffers.Builder, field int, off flatbuffers.UOffsetT) {
	if off != 0 {
		b.PrependUOffsetTSlot(field, off, 0)
	}
}

// WriteSegment serializes d. Callers must have filled the derived fields (see
// index.finishSegment).
func WriteSegment(d *SegmentData) []byte {
	b := flatbuffers.NewBuilder(1 << 20)
	offs := [segNumFields]flatbuffers.UOffsetT{
		segHashes:          byteVec(b, d.Hashes),
		segHashOrder:       u32Vec(b, d.HashOrder),
		segAuthors:         u32Vec(b, d.Authors),
		segTimes:           i64Vec(b, d.Times),
		segParentOffsets:   u32Vec(b, d.ParentOffsets),
		segParents:         u32Vec(b, d.Parents),
		segSubjects:        strVec(b, d.Subjects),
		segChangeOffsets:   u32Vec(b, d.ChangeOffsets),
		segChangePaths:     u32Vec(b, d.ChangePaths),
		segChangeKinds:     byteVec(b, d.ChangeKinds),
		segChangeModeCodes: byteVec(b, d.ChangeModeCodes),
		segRenameChanges:   u32Vec(b, d.RenameChanges),
		segRenameOldPaths:  u32Vec(b, d.RenameOldPaths),
		segPHPaths:         u32Vec(b, d.PHPaths),
		segPHOffsets:       u32Vec(b, d.PHOffsets),
		segPHCommits:       u32Vec(b, d.PHCommits),
		segStrings:         strVec(b, d.Strings),
		segStringHashes:    u64Vec(b, d.StringHashes),
		segStringHashIDs:   u32Vec(b, d.StringHashIDs),
		segTRTests:         u32Vec(b, d.TRTests),
		segTRUnits:         u32Vec(b, d.TRUnits),
		segTRCommits:       u32Vec(b, d.TRCommits),
		segTRTimes:         i64Vec(b, d.TRTimes),
		segTRStatus:        byteVec(b, d.TRStatus),
		segTRDurations:     u32Vec(b, d.TRDurations),
		segTRJobs:          u32Vec(b, d.TRJobs),
	}
	b.StartObject(segNumFields)
	b.PrependUint32Slot(segVersion, Version, 0)
	b.PrependUint32Slot(segBaseCommit, d.BaseCommit, 0)
	b.PrependUint32Slot(segBaseString, d.BaseString, 0)
	for f, o := range offs {
		addOffset(b, f, o)
	}
	b.Finish(b.EndObject())
	return b.FinishedBytes()
}

// Reach field numbers.
const (
	reachNumCommits = iota
	reachRefs
	reachTips
	reachWordsPerCommit
	reachBitmap
	reachChildrenOffsets
	reachChildren
	reachNumFields
)

// Reach is a zero-copy view of the reachability overlay.
type Reach struct {
	NumCommits      uint32
	Refs            Strings
	Tips            []uint32
	WordsPerCommit  uint32
	Bitmap          []uint64
	ChildrenOffsets []uint32
	Children        []uint32
}

func ReadReach(buf []byte) (*Reach, error) {
	t, err := rootTable(buf)
	if err != nil {
		return nil, err
	}
	return &Reach{
		NumCommits:      t.u32(reachNumCommits),
		Refs:            t.strs(reachRefs),
		Tips:            t.u32s(reachTips),
		WordsPerCommit:  t.u32(reachWordsPerCommit),
		Bitmap:          t.u64s(reachBitmap),
		ChildrenOffsets: t.u32s(reachChildrenOffsets),
		Children:        t.u32s(reachChildren),
	}, nil
}

type ReachData struct {
	NumCommits      uint32
	Refs            []string
	Tips            []uint32
	WordsPerCommit  uint32
	Bitmap          []uint64
	ChildrenOffsets []uint32
	Children        []uint32
}

func WriteReach(d *ReachData) []byte {
	b := flatbuffers.NewBuilder(1 << 16)
	offs := [reachNumFields]flatbuffers.UOffsetT{
		reachRefs:            strVec(b, d.Refs),
		reachTips:            u32Vec(b, d.Tips),
		reachBitmap:          u64Vec(b, d.Bitmap),
		reachChildrenOffsets: u32Vec(b, d.ChildrenOffsets),
		reachChildren:        u32Vec(b, d.Children),
	}
	b.StartObject(reachNumFields)
	b.PrependUint32Slot(reachNumCommits, d.NumCommits, 0)
	b.PrependUint32Slot(reachWordsPerCommit, d.WordsPerCommit, 0)
	for f, o := range offs {
		addOffset(b, f, o)
	}
	b.Finish(b.EndObject())
	return b.FinishedBytes()
}

// OldPath returns the old path ID of change j (an index into the segment's
// change vectors) if it is a rename or copy.
func (s *Segment) OldPath(j uint32) (uint32, bool) {
	rc := s.RenameChanges
	lo, hi := 0, len(rc)
	for lo < hi {
		m := (lo + hi) / 2
		if rc[m] < j {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < len(rc) && rc[lo] == j {
		return s.RenameOldPaths[lo], true
	}
	return NoPath, false
}

// Git modes, coded in one byte. Anything unusual is kept as ModeOther.
var modeCodes = []uint32{0, 0o100644, 0o100755, 0o120000, 0o160000, 0o040000}

const ModeOther = 255

func ModeCode(mode uint32) byte {
	for i, m := range modeCodes {
		if m == mode {
			return byte(i)
		}
	}
	return ModeOther
}

// Mode decodes a mode code; ModeOther decodes to 0.
func Mode(code byte) uint32 {
	if int(code) < len(modeCodes) {
		return modeCodes[code]
	}
	return 0
}

// ---- Blobs sidecar ----

// Blobs is an optional sidecar for one core segment: the new blob hash of
// each of that segment's changes, in the same order. Consumers that never ask
// "has this content been here before" do not need to fetch it.
const (
	blobsVersion = iota
	blobsBaseCommit
	blobsNumChanges
	blobsHashes
	blobsNumFields
)

type Blobs struct {
	BaseCommit uint32
	NumChanges uint32
	Hashes     []byte // 20 bytes per change
}

func ReadBlobs(buf []byte) (*Blobs, error) {
	t, err := rootTable(buf)
	if err != nil {
		return nil, err
	}
	if v := t.u32(blobsVersion); v != Version {
		return nil, fmt.Errorf("%w: blobs sidecar is v%d, this build reads v%d", ErrVersion, v, Version)
	}
	b := &Blobs{BaseCommit: t.u32(blobsBaseCommit), NumChanges: t.u32(blobsNumChanges), Hashes: t.bytes(blobsHashes)}
	if len(b.Hashes) != 20*int(b.NumChanges) {
		return nil, fmt.Errorf("corrupt blobs sidecar: %d changes, %d hash bytes", b.NumChanges, len(b.Hashes))
	}
	return b, nil
}

func WriteBlobs(baseCommit uint32, hashes []byte) []byte {
	b := flatbuffers.NewBuilder(len(hashes) + 64)
	h := b.CreateByteVector(hashes)
	b.StartObject(blobsNumFields)
	b.PrependUint32Slot(blobsVersion, Version, 0)
	b.PrependUint32Slot(blobsBaseCommit, baseCommit, 0)
	b.PrependUint32Slot(blobsNumChanges, uint32(len(hashes)/20), 0)
	b.PrependUOffsetTSlot(blobsHashes, h, 0)
	b.Finish(b.EndObject())
	return b.FinishedBytes()
}

// ---- Hunks sidecar ----

// Hunks is an optional sidecar for one core segment: for each change, the
// line ranges the first-parent diff replaced (from `git diff -U0`), in the
// core segment's change order. It turns blame into index lookups: a line's
// number at a commit is mapped back through each older commit's hunks, with
// no file contents needed.
const (
	hunksVersion = iota
	hunksBaseCommit
	hunksNumChanges
	hunksChangeOffsets
	hunksOldStart
	hunksOldLen
	hunksNewStart
	hunksNewLen
	hunksNumFields
)

type Hunks struct {
	BaseCommit    uint32
	ChangeOffsets []uint32 // num_changes+1 offsets into the hunk vectors
	OldStart      []uint32
	OldLen        []uint32
	NewStart      []uint32
	NewLen        []uint32
}

type HunksData struct {
	ChangeOffsets                      []uint32
	OldStart, OldLen, NewStart, NewLen []uint32
}

func ReadHunks(buf []byte) (*Hunks, error) {
	t, err := rootTable(buf)
	if err != nil {
		return nil, err
	}
	if v := t.u32(hunksVersion); v != Version {
		return nil, fmt.Errorf("%w: hunks sidecar is v%d, this build reads v%d", ErrVersion, v, Version)
	}
	h := &Hunks{
		BaseCommit:    t.u32(hunksBaseCommit),
		ChangeOffsets: t.u32s(hunksChangeOffsets),
		OldStart:      t.u32s(hunksOldStart),
		OldLen:        t.u32s(hunksOldLen),
		NewStart:      t.u32s(hunksNewStart),
		NewLen:        t.u32s(hunksNewLen),
	}
	n := len(h.OldStart)
	if len(h.ChangeOffsets) != int(t.u32(hunksNumChanges))+1 || len(h.OldLen) != n || len(h.NewStart) != n || len(h.NewLen) != n {
		return nil, fmt.Errorf("corrupt hunks sidecar")
	}
	return h, nil
}

func WriteHunks(baseCommit uint32, d *HunksData) []byte {
	b := flatbuffers.NewBuilder(16*len(d.OldStart) + 4*len(d.ChangeOffsets) + 64)
	empty := func(v []uint32) []uint32 {
		if v == nil {
			return []uint32{}
		}
		return v
	}
	offs := [hunksNumFields]flatbuffers.UOffsetT{
		hunksChangeOffsets: u32Vec(b, empty(d.ChangeOffsets)),
		hunksOldStart:      u32Vec(b, empty(d.OldStart)),
		hunksOldLen:        u32Vec(b, empty(d.OldLen)),
		hunksNewStart:      u32Vec(b, empty(d.NewStart)),
		hunksNewLen:        u32Vec(b, empty(d.NewLen)),
	}
	b.StartObject(hunksNumFields)
	b.PrependUint32Slot(hunksVersion, Version, 0)
	b.PrependUint32Slot(hunksBaseCommit, baseCommit, 0)
	b.PrependUint32Slot(hunksNumChanges, uint32(len(d.ChangeOffsets)-1), 0)
	for f, o := range offs {
		addOffset(b, f, o)
	}
	b.Finish(b.EndObject())
	return b.FinishedBytes()
}
