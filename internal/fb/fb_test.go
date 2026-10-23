package fb

import (
	"slices"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
)

func TestSegmentRoundTrip(t *testing.T) {
	d := &SegmentData{
		BaseCommit: 7, BaseString: 3,
		Hashes:        make([]byte, 40),
		HashOrder:     []uint32{1, 0},
		Authors:       []uint32{3, 4},
		Times:         []int64{100, -5},
		ParentOffsets: []uint32{0, 0, 1},
		Parents:       []uint32{7},
		Subjects:      []string{"first", "second ✓"},
		ChangeOffsets: []uint32{0, 1, 1},
		ChangePaths:   []uint32{3}, ChangeKinds: []byte{'R'}, ChangeModeCodes: []byte{ModeCode(0o100644)},
		RenameChanges: []uint32{0}, RenameOldPaths: []uint32{4},
		Strings:      []string{"a.go", "Ann <a@x>"},
		StringHashes: []uint64{1, 2}, StringHashIDs: []uint32{3, 4},
	}
	d.Hashes[20] = 0xff
	s, err := ReadSegment(WriteSegment(d))
	if err != nil {
		t.Fatal(err)
	}
	if s.BaseCommit != 7 || s.NumCommits() != 2 || s.Hashes[20] != 0xff || !slices.Equal(s.Times, d.Times) ||
		s.Subjects.At(1) != "second ✓" || s.Strings.At(0) != "a.go" || Mode(s.ChangeModeCodes[0]) != 0o100644 {
		t.Errorf("round trip lost data: %+v", s)
	}
	if s.TRTests != nil {
		t.Errorf("absent vector should read as nil, got %v", s.TRTests)
	}
	if old, ok := s.OldPath(0); !ok || old != 4 {
		t.Errorf("OldPath(0) = %d %v, want 4 true", old, ok)
	}
	if _, ok := s.OldPath(1); ok {
		t.Error("change 1 is not a rename")
	}
}

func TestBlobsRoundTrip(t *testing.T) {
	h := make([]byte, 40)
	h[39] = 7
	b, err := ReadBlobs(WriteBlobs(5, h))
	if err != nil {
		t.Fatal(err)
	}
	if b.BaseCommit != 5 || b.NumChanges != 2 || b.Hashes[39] != 7 {
		t.Errorf("blobs round trip = %+v", b)
	}
}

func TestModeCodes(t *testing.T) {
	for _, m := range []uint32{0, 0o100644, 0o100755, 0o120000, 0o160000, 0o040000} {
		if Mode(ModeCode(m)) != m {
			t.Errorf("mode %o does not round-trip", m)
		}
	}
	if ModeCode(0o100600) != ModeOther {
		t.Error("unusual modes should code as ModeOther")
	}
}

// A segment written by a future version, with a field this reader does not
// know, must still read. This is FlatBuffers' forward-compatibility rule, and
// the schema policy: fields are only appended, never removed or reordered.
func TestFutureFieldIgnored(t *testing.T) {
	b := flatbuffers.NewBuilder(256)
	future := u32Vec(b, []uint32{42, 43}) // e.g. a hunks table at field segNumFields
	times := i64Vec(b, []int64{9})
	po := u32Vec(b, []uint32{0, 0})
	co := u32Vec(b, []uint32{0, 0})
	hashes := byteVec(b, make([]byte, 20))
	b.StartObject(segNumFields + 1)
	b.PrependUint32Slot(segVersion, Version, 0)
	b.PrependUOffsetTSlot(segHashes, hashes, 0)
	b.PrependUOffsetTSlot(segTimes, times, 0)
	b.PrependUOffsetTSlot(segParentOffsets, po, 0)
	b.PrependUOffsetTSlot(segChangeOffsets, co, 0)
	b.PrependUOffsetTSlot(segNumFields, future, 0)
	b.Finish(b.EndObject())

	s, err := ReadSegment(b.FinishedBytes())
	if err != nil {
		t.Fatal(err)
	}
	if s.NumCommits() != 1 || s.Times[0] != 9 {
		t.Errorf("known fields misread next to an unknown one: %+v", s)
	}
}

func TestNewerMajorVersionRejected(t *testing.T) {
	b := flatbuffers.NewBuilder(64)
	b.StartObject(segNumFields)
	b.PrependUint32Slot(segVersion, Version+1, 0)
	b.Finish(b.EndObject())
	if _, err := ReadSegment(b.FinishedBytes()); err == nil {
		t.Error("a breaking format version must be refused, not misread")
	}
}
