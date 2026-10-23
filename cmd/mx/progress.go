package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	gosync "sync"
	"time"

	"golang.org/x/term"

	"github.com/tfcace/marlex/internal/status"
	"github.com/tfcace/marlex/internal/viz"
)

// job shows a long-running command's progress twice: as a framed meter
// redrawn in place on stderr (when it is a terminal), and as an OSC 7501
// record, so Rex shows the percentage on the tab while you look elsewhere.
// Log lines become the meter's caption instead of scrolling past it.
type job struct {
	mu      gosync.Mutex
	id      string // the record's id, e.g. mx/build
	title   string
	what    string // what is counted, e.g. "commits"
	st      *status.Reporter
	tty     bool
	color   bool
	width   int
	drawn   int // lines on screen from the last draw
	last    time.Time
	started time.Time
	done    int
	total   int
	caption string
}

func newJob(id, title, what string) *job {
	j := &job{id: id, title: title, what: what, st: status.New(), started: time.Now()}
	if term.IsTerminal(int(os.Stderr.Fd())) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" {
		j.tty, j.color = true, true
		if c, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil {
			j.width = min(c, 72)
		}
		if j.width < 40 {
			j.tty = false
		}
	}
	j.st.Report(status.Report{State: status.Working, ID: id, Progress: -1, Title: title, Msg: title})
	return j
}

// Write takes the build's log lines: the caption on a terminal, else
// stderr as before.
func (j *job) Write(p []byte) (int, error) {
	if !j.tty {
		return os.Stderr.Write(p)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if s := strings.TrimSpace(string(p)); s != "" {
		j.caption = s
	}
	j.draw(false)
	return len(p), nil
}

// Progress records how much is done. Draws are at most every 50 ms, and
// status reports only when the percentage changes.
func (j *job) Progress(done, total int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	before := j.percent()
	j.done, j.total = done, total
	if p := j.percent(); p != before {
		j.st.Report(status.Report{State: status.Working, ID: j.id, Progress: p, Title: j.title,
			Msg: fmt.Sprintf("%s: %s of %s %s", j.title, comma(done), comma(total), j.what)})
	}
	j.draw(false)
}

func (j *job) percent() int {
	if j.total == 0 {
		return -1
	}
	return j.done * 100 / j.total
}

func (j *job) draw(final bool) {
	if !j.tty || (!final && time.Since(j.last) < 50*time.Millisecond) {
		return
	}
	j.last = time.Now()
	var blocks []viz.Block
	if j.total > 0 {
		blocks = append(blocks, viz.Meter{Value: float64(j.done) / float64(j.total), Text: fmt.Sprintf("%d%%", j.percent())})
		blocks = append(blocks, viz.Text{viz.M(fmt.Sprintf("%s of %s %s · %s", comma(j.done), comma(j.total), j.what, time.Since(j.started).Round(100*time.Millisecond)))})
	}
	cap := j.caption
	if cap == "" {
		cap = " "
	}
	blocks = append(blocks, viz.Text{viz.M(cap)})
	j.paint(viz.Figure{Title: j.title, Blocks: blocks}.Render(j.width, 1, j.color))
}

func (j *job) paint(lines []string) { j.drawn = repaint(os.Stderr, j.drawn, lines) }

// repaint draws lines over the drawn lines a previous call left above the
// cursor, and returns how many it drew.
func repaint(w io.Writer, drawn int, lines []string) int {
	var b strings.Builder
	if drawn > 0 {
		fmt.Fprintf(&b, "\r\x1b[%dA", drawn)
	}
	for _, l := range lines {
		b.WriteString("\x1b[2K" + l + "\n")
	}
	if drawn > len(lines) { // a shorter figure: clear what the last one left
		b.WriteString("\x1b[J")
	}
	io.WriteString(w, b.String())
	return len(lines)
}

// Finish replaces the meter with the outcome. A job that took long enough
// for you to look away leaves a done record (Rex marks the tab until you
// see it); a quick one clears its record. A failure leaves an error.
func (j *job) Finish(summary string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	elapsed := time.Since(j.started)
	switch {
	case err != nil:
		j.st.Report(status.Report{State: status.Error, ID: j.id, Progress: -1, Title: j.title, Msg: j.title + " failed: " + err.Error()})
	case elapsed > 10*time.Second:
		j.st.Report(status.Report{State: status.Done, ID: j.id, Progress: -1, Title: j.title, Msg: summary})
	default:
		j.st.Report(status.Report{State: status.Clear, ID: j.id})
	}
	j.st.Close()
	if !j.tty || j.drawn == 0 {
		return
	}
	if err != nil {
		j.paint(nil) // the error is printed after
		return
	}
	j.paint(viz.Figure{Title: j.title, Blocks: []viz.Block{
		viz.Meter{Value: 1, Text: "done"},
		viz.Text{viz.M(summary)},
	}}.Render(j.width, 1, j.color))
}

func comma(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Print writes the summary to stdout, unless the finished meter already
// shows it on the same terminal.
func (j *job) Print(summary string) {
	if j.drawn > 0 && stdoutTTY() {
		return
	}
	fmt.Println(summary)
}

// log is where a build's own log lines go: the meter's caption on a
// terminal, nowhere otherwise, for jobs that say what they do themselves.
func (j *job) log() io.Writer {
	if j.tty {
		return j
	}
	return io.Discard
}
