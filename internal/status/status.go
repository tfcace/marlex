// Package status reports what mx is doing to the terminal with the
// Program Status Protocol (OSC 7501):
// https://www.superlogical.com/rex/docs/build/program-status
//
// A report is OSC 7501 ; key=value:key=value ST. Rex shows the records in
// tab headers and the session picker, so a build in a background tab is
// visible without switching to it. mx
// only writes reports. The encoder and the text fitting follow hash's
// programstatus package.
package status

import (
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits from the specification. A terminal discards a report that breaks
// one, so fields are fitted before encoding.
const (
	maxMsg     = 2048
	maxTitle   = 192
	maxSegment = 32
)

type State string

const (
	Idle    State = "idle"
	Working State = "working"
	Done    State = "done"
	Blocked State = "blocked"
	Error   State = "error"
	Clear   State = "clear"
)

// Report is one OSC 7501 report. A report replaces its record completely.
type Report struct {
	State State
	ID    string // slash-separated, e.g. mx/build
	// Progress is 0 to 100 for Working; -1 (or any negative) leaves it out,
	// which means indeterminate.
	Progress int
	Title    string
	Msg      string
}

// Encode builds the body of a report, without OSC and ST.
func Encode(r Report) string {
	pairs := []string{"state=" + string(r.State)}
	if r.ID != "" {
		pairs = append(pairs, "id="+fitID(r.ID))
	}
	if r.State == Clear {
		return strings.Join(pairs, ":")
	}
	if r.Progress >= 0 && (r.State == Working || r.State == Blocked) {
		pairs = append(pairs, "progress="+strconv.Itoa(min(r.Progress, 100)))
	}
	pairs = append(pairs, "app=mx")
	if t := fitLine(r.Title, maxTitle); t != "" {
		pairs = append(pairs, "title="+base64.StdEncoding.EncodeToString([]byte(t)))
	}
	if m := fitLine(r.Msg, maxMsg); m != "" {
		pairs = append(pairs, "msg="+base64.StdEncoding.EncodeToString([]byte(m)))
	}
	return strings.Join(pairs, ":")
}

// Sequence is Encode wrapped in OSC 7501 and ESC \.
func Sequence(r Report) string {
	return "\x1b]7501;" + Encode(r) + "\x1b\\"
}

// fitLine makes s one line of at most limit bytes: control characters and
// runs of whitespace become one space, invalid UTF-8 is dropped, and the cut
// falls on a rune boundary.
func fitLine(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError:
			return -1
		case r <= 0x1f || (r >= 0x7f && r <= 0x9f):
			return ' '
		}
		return r
	}, s)
	var b strings.Builder
	for _, r := range strings.Join(strings.Fields(s), " ") {
		if b.Len()+utf8.RuneLen(r) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fitID makes each segment of id valid: [A-Za-z0-9_.+-], at most 32 bytes.
func fitID(id string) string {
	segs := strings.Split(id, "/")
	for i, s := range segs {
		b := []byte(s)
		for j, c := range b {
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '+' || c == '-'
			if !ok {
				b[j] = '-'
			}
		}
		if len(b) > maxSegment {
			b = b[:maxSegment]
		}
		if len(b) == 0 {
			b = []byte("-")
		}
		segs[i] = string(b)
	}
	return strings.Join(segs, "/")
}

// Reporter writes reports. A nil Reporter writes nothing, so callers never
// check for one.
type Reporter struct {
	w io.Writer
	c io.Closer
}

// New returns a Reporter for the terminal mx runs in, or nil when
// reports are off. They are on in Rex (TERM_PROGRAM=rex) or with
// MX_STATUS=1, and off with MX_STATUS=0. They go to stderr when
// it is a terminal; otherwise, as in an agent's hook whose output is
// captured, to the controlling terminal.
func New() *Reporter {
	switch os.Getenv("MX_STATUS") {
	case "0", "off", "false":
		return nil
	case "1", "on", "true":
	default:
		if os.Getenv("TERM_PROGRAM") != "rex" {
			return nil
		}
	}
	if isTerminal(os.Stderr) {
		return &Reporter{w: os.Stderr}
	}
	if f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		return &Reporter{w: f, c: f}
	}
	// Agent hooks run without a controlling terminal (Claude Code starts
	// them in their own session), but the agent itself has one: report to
	// the terminal device of the nearest ancestor that has a terminal.
	if dev := ancestorTTY(); dev != "" {
		if f, err := os.OpenFile(dev, os.O_WRONLY, 0); err == nil {
			return &Reporter{w: f, c: f}
		}
	}
	return nil
}

// ancestorTTY walks up the process tree, at most eight levels, to the
// first process with a terminal and returns its device path.
func ancestorTTY() string {
	pid := os.Getppid()
	for i := 0; i < 8 && pid > 1; i++ {
		out, err := exec.Command("ps", "-o", "ppid=,tty=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return ""
		}
		f := strings.Fields(string(out))
		if len(f) != 2 {
			return ""
		}
		if tty := f[1]; tty != "??" && tty != "?" && tty != "-" {
			if !strings.HasPrefix(tty, "/") {
				tty = "/dev/" + tty
			}
			return tty
		}
		pid, _ = strconv.Atoi(f[0])
	}
	return ""
}

// To returns a Reporter writing to w, for tests.
func To(w io.Writer) *Reporter { return &Reporter{w: w} }

func (r *Reporter) Report(rep Report) {
	if r == nil {
		return
	}
	io.WriteString(r.w, Sequence(rep)) //nolint:errcheck // terminal escapes are best effort
}

func (r *Reporter) Close() {
	if r != nil && r.c != nil {
		r.c.Close()
	}
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
