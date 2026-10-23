package status

import (
	"strings"
	"testing"
)

func TestEncode(t *testing.T) {
	cases := []struct {
		r    Report
		want string
	}{
		{Report{State: Working, ID: "mx/build", Progress: 42, Msg: "ok"}, "state=working:id=mx/build:progress=42:app=mx:msg=b2s="},
		{Report{State: Working, ID: "mx/build", Progress: -1}, "state=working:id=mx/build:app=mx"},
		{Report{State: Done, ID: "mx/tests", Progress: 100, Title: "tests"}, "state=done:id=mx/tests:app=mx:title=dGVzdHM="},
		{Report{State: Clear, ID: "mx/build", Msg: "dropped"}, "state=clear:id=mx/build"},
		{Report{State: Working, ID: "mx/a b:c", Progress: 250}, "state=working:id=mx/a-b-c:progress=100:app=mx"},
	}
	for _, c := range cases {
		if got := Encode(c.r); got != c.want {
			t.Errorf("Encode(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

// Free text is fitted the way the specification requires: one line, no
// control characters, cut on a rune boundary within the limit.
func TestFitLine(t *testing.T) {
	if got := fitLine("a\tb\n\x07c", 100); got != "a b c" {
		t.Errorf("fitLine = %q", got)
	}
	long := strings.Repeat("é", 2000) // 4000 bytes
	if got := fitLine(long, maxMsg); len(got) != maxMsg || !strings.HasSuffix(got, "é") {
		t.Errorf("fitLine cut to %d bytes", len(got))
	}
}

func TestNilReporter(t *testing.T) {
	var r *Reporter
	r.Report(Report{State: Working})
	r.Close()
	var b strings.Builder
	To(&b).Report(Report{State: Idle})
	if b.String() != "\x1b]7501;state=idle:app=mx\x1b\\" {
		t.Errorf("wrote %q", b.String())
	}
}
