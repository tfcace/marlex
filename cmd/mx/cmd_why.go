package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tfcace/marlex/internal/index"
	"github.com/tfcace/marlex/internal/screen"
	"github.com/tfcace/marlex/internal/viz"
)

// view is one thing a card describes: a file, or a line of it, as of a
// commit. Digging replaces it with the same line as of an older commit.
type view struct {
	path string
	line int    // 0 for the whole file
	at   uint32 // the commit the file is read at
	head bool   // at is what HEAD stands on, so the working tree is the text
	note string // why this view was chosen, e.g. "before a1b2c3d4e5"
	// newer is the version this one was replaced by, when the view is an
	// older version of a line: its text, line number, and the commit that
	// made the change.
	newer *newerVersion
}

type newerVersion struct {
	text   string
	textOK bool
	line   int
	commit index.Commit
}

// whySource is a repository and its open index. References on one screen
// can point into different repositories (an absolute path in a stack
// trace), so each reference carries its own.
type whySource struct {
	e      env
	ix     *index.Index
	r      *gitRepo
	byBase map[string][]string // file name -> paths with history, built on first use
}

// target is a reference that resolved to a file with history.
type whyRef struct {
	src  *whySource
	raw  string // as it appeared on screen
	path string
	line int
	row  int    // the screen row it came from, -1 when given as an argument
	hint string // what would let mx find the line, when it couldn't
}

func cmdWhy(e env, args []string) error {
	fs := newFlags("why", "FILE[:LINE] [-i] [--json]")
	screenFile := fs.String("screen", "", "find file references in a saved terminal screen")
	cursor := fs.String("cursor", "", "ROW,COL of the cursor on that screen, 1-based")
	cols := fs.Int("cols", 0, "the screen's width, to rejoin rows it wrapped")
	rows := fs.Int("rows", 0, "the screen's height")
	rmScreen := fs.Bool("rm-screen", false, "delete the --screen file once read")
	program := fs.String("program", "", "the screen's foreground program; for an editor, the file and line it shows come first")
	pid := fs.Int("pid", 0, "the screen's foreground process, to find an editor running under it")
	title := fs.String("title", "", "the terminal's title, where an editor may name its file and line")
	follow := fs.String("follow", "", "with -i in Rex: the block the screen came from, to follow as it changes (an editor's cursor)")
	session := fs.String("session", "", "the Rex session of --follow's block")
	interactive := fs.Bool("i", false, "stay open: down shows the line's previous version, left and right the other files on screen")
	asJSON := fs.Bool("json", false, "print JSON")
	hide(fs, "screen", "cursor", "cols", "rows", "rm-screen", "program", "pid", "title", "follow", "session")
	pos := parse(fs, args)
	if (*screenFile == "") == (len(pos) == 0) || len(pos) > 1 {
		return usageErr(fs)
	}

	sources := map[string]*whySource{}
	defer func() {
		for _, s := range sources {
			s.ix.Close()
		}
	}()
	fromScreen := func(st screenState) func() ([]whyRef, error) {
		return func() ([]whyRef, error) {
			cur := ""
			if st.Row > 0 {
				cur = fmt.Sprintf("%d,%d", st.Row, st.Col)
			}
			return refsOnScreen(e, sources, st.Text, cur, st.Cols, st.Rows, st.editor(), st.Title)
		}
	}
	load := func() ([]whyRef, error) { return refFromArg(e, sources, pos[0]) }
	if *screenFile != "" {
		data, err := os.ReadFile(*screenFile)
		if *rmScreen {
			os.Remove(*screenFile)
		}
		if err != nil {
			return err
		}
		row, col := 0, 0
		fmt.Sscanf(*cursor, "%d,%d", &row, &col)
		load = fromScreen(screenState{Text: string(data), Row: row, Col: col, Rows: *rows, Cols: *cols, Program: *program, Pid: *pid, Title: *title})
	}
	refs, err := load()
	// A line question needs line ranges: add them now, once, if the index
	// has none, rather than answer without the line.
	for _, r := range refs {
		if r.line > 0 && !r.src.ix.HasHunks() {
			r.src.ix = r.src.e.ensureHunks(r.src.ix)
		}
	}
	var fol *following
	if *follow != "" {
		fol = &following{block: *follow, session: *session, load: fromScreen}
	}
	if *asJSON {
		if err != nil {
			return err
		}
		return printJSON(cardJSON(refs[0]))
	}
	if *interactive {
		return runWhyUI(e, load, fol, refs, err)
	}
	if err != nil {
		return err
	}
	cols0, rows0 := termSize()
	cols0 = min(cols0, 160) // printed into a wide terminal, stay readable
	c := newCard(refs[0])
	lines := func(t float64) []string { return c.render(cols0, 0, t, colorOn()) }
	viz.Animate(os.Stdout, lines, motionOn() && len(lines(1)) < rows0, false)
	return nil
}

// source opens the repository that contains dir, once per repository.
func source(e env, sources map[string]*whySource, dir string) (*whySource, error) {
	e2 := e
	e2.repo = dir
	e2.st = &envState{}
	r, err := e2.gitRepo()
	if err != nil {
		return nil, err
	}
	if s, ok := sources[r.root]; ok {
		return s, nil
	}
	ix, err := e2.open()
	if errors.Is(err, errNoIndex) {
		return nil, needsIndex{e: e2, root: r.root, shallow: r.shallow()}
	}
	if err != nil {
		return nil, err
	}
	s := &whySource{e: e2, ix: ix, r: r}
	sources[r.root] = s
	return s, nil
}

// editorHint is the setting that makes an editor show its file and line.
func editorHint(name string) string {
	switch name {
	case "vim", "vi", "view", "nvim":
		return "Add this to your vimrc and mx follows the cursor exactly: set title titlestring=%f:%l"
	case "nano":
		return "Add set constantshow to your .nanorc, so nano shows the line."
	case "less":
		return "Set LESS=-M in your environment, so less shows the file and its lines."
	}
	return "Turn on its status line, with the file name and the cursor's position."
}

// needsIndex is a repository without an index, which the interactive view
// offers to build.
type needsIndex struct {
	e       env
	root    string
	shallow bool
}

func (n needsIndex) Error() string {
	if n.shallow {
		return filepath.Base(n.root) + " is a shallow clone without an index: mx pull URL brings a published one."
	}
	return filepath.Base(n.root) + " has no index yet: mx build makes one, in seconds for most repositories."
}

func refFromArg(e env, sources map[string]*whySource, arg string) ([]whyRef, error) {
	src, err := source(e, sources, e.repo)
	if err != nil {
		return nil, err
	}
	p, line := arg, 0
	if i := strings.LastIndexByte(arg, ':'); i > 0 {
		if n, err := strconv.Atoi(arg[i+1:]); err == nil && n > 0 {
			p, line = arg[:i], n
		}
	}
	t, err := e.resolvePath(src.ix, p)
	if err != nil {
		return nil, err
	}
	if t.Dir {
		return nil, fmt.Errorf("why takes a file; %s is a directory", t)
	}
	return []whyRef{{src: src, raw: arg, path: t.Path, line: line, row: -1}}, nil
}

// refsOnScreen reads a screen's text and returns the references on it that
// name files with history, nearest the cursor first.
func refsOnScreen(e env, sources map[string]*whySource, data, cursor string, cols, rows int, ed editorProc, title string) ([]whyRef, error) {
	lines := strings.Split(strings.TrimRight(data, "\n"), "\n")
	// The screen is the last rows lines of the dump; the rest is scrollback.
	top := 0
	if rows > 0 && len(lines) > rows {
		top = len(lines) - rows
	}
	crow, ccol := len(lines)-1, 0
	if r, c, ok := strings.Cut(cursor, ","); ok {
		rn, _ := strconv.Atoi(r)
		cn, _ := strconv.Atoi(c)
		crow, ccol = min(top+rn-1, len(lines)-1), max(cn-1, 0)
	}
	found := screen.ByDistance(screen.Find(lines, cols), crow, ccol)
	editing, hint := "", ""
	if ed.name != "" {
		// In an editor, the file and cursor line it shows are the only
		// reference: the view follows the cursor, and the other names on
		// screen (buffer tabs, code) are not what the reader is looking at.
		userHome, _ := os.UserHomeDir()
		r, ok := screen.Editor(lines[top:], crow-top, title, userHome)
		if !ok {
			return nil, fmt.Errorf("%s isn't showing which file it has open. %s", ed.name, editorHint(ed.name))
		}
		r.Row = crow
		found, editing = []screen.Ref{r}, r.Path
		if r.Line == 0 {
			hint = fmt.Sprintf("%s isn't showing the cursor's line, so this is the whole file. %s", ed.name, editorHint(ed.name))
		}
	}

	home, homeErr := source(e, sources, e.repo)
	var out []whyRef
	seen := map[string]bool{}
	for _, f := range found {
		src := home
		if filepath.IsAbs(f.Path) && (src == nil || !within(src.r.root, f.Path)) {
			if s, err := source(e, sources, filepath.Dir(f.Path)); err == nil {
				src = s
			}
		}
		if src == nil {
			continue
		}
		p, ok := src.resolveRef(f.Path)
		if !ok {
			continue
		}
		// A file named twice, as an editor's buffer tab and its status
		// line, is one reference: a whole-file mention adds nothing to a
		// line of the same file.
		key := src.r.root + "\x00" + p + ":" + strconv.Itoa(f.Line)
		file := src.r.root + "\x00" + p + ":0"
		if seen[key] || (f.Line == 0 && seen[file]) {
			continue
		}
		seen[key], seen[file] = true, true
		out = append(out, whyRef{src: src, raw: f.Path, path: p, line: f.Line, row: f.Row, hint: hint})
		if len(out) == 50 {
			break
		}
	}
	if len(out) == 0 {
		var ni needsIndex
		if errors.As(homeErr, &ni) && len(sources) == 0 {
			return nil, ni
		}
		if editing != "" && homeErr == nil {
			return nil, fmt.Errorf("%s has no history in the index yet: a new or untracked file, or one changed since the last commit mx indexed", editing)
		}
		if homeErr != nil && len(sources) == 0 {
			return nil, fmt.Errorf("no file on screen has history, and %v", homeErr)
		}
		return nil, fmt.Errorf("none of the %d file names on screen has history in the index", len(found))
	}
	return out, nil
}

func within(root, p string) bool {
	if d, err := filepath.EvalSymlinks(p); err == nil {
		p = d
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && !strings.HasPrefix(rel, "..")
}

// resolveRef finds the file a reference names: as a path (relative to the
// terminal's directory or the repository's root), without a diff's a/ or b/
// prefix, or else by its trailing components, which is how go test and
// stack traces from other machines write them.
func (s *whySource) resolveRef(raw string) (string, bool) {
	tries := []string{raw}
	if r, ok := strings.CutPrefix(raw, "a/"); ok {
		tries = append(tries, r)
	} else if r, ok := strings.CutPrefix(raw, "b/"); ok {
		tries = append(tries, r)
	}
	for _, t := range tries {
		for _, c := range s.e.pathCandidates(t) {
			if s.ix.HasHistory(c) {
				return c, true
			}
		}
	}
	if s.byBase == nil {
		s.byBase = map[string][]string{}
		for _, id := range s.ix.PathsUnder("") {
			p := s.ix.String(id)
			s.byBase[path.Base(p)] = append(s.byBase[path.Base(p)], p)
		}
	}
	parts := strings.Split(strings.Trim(path.Clean(filepath.ToSlash(raw)), "/"), "/")
	matches := s.byBase[parts[len(parts)-1]]
	if len(matches) == 0 {
		return "", false
	}
	// Keep the matches that share the most trailing components.
	best, bestN := []string(nil), 0
	for _, m := range matches {
		mp := strings.Split(m, "/")
		n := 0
		for n < len(parts) && n < len(mp) && parts[len(parts)-1-n] == mp[len(mp)-1-n] {
			n++
		}
		switch {
		case n > bestN:
			best, bestN = []string{m}, n
		case n == bestN:
			best = append(best, m)
		}
	}
	// Then prefer files under the terminal's directory, then shorter paths.
	here := ""
	if c := s.e.pathCandidates("."); len(c) > 0 && c[0] != "." {
		here = c[0] + "/"
	}
	sort.SliceStable(best, func(i, j int) bool {
		hi, hj := strings.HasPrefix(best[i], here), strings.HasPrefix(best[j], here)
		if hi != hj {
			return hi
		}
		return len(best[i]) < len(best[j])
	})
	return best[0], true
}

// card is everything a view shows, gathered once.
type card struct {
	ref    whyRef
	v      view
	err    string // why there is no line blame, when there isn't
	text   string // the line's text
	textOK bool   // whether text could be read

	blame   *index.BlameLine
	commit  index.Commit
	lines   int // the file's length at v.at
	log     []index.LogEntry
	created bool // the oldest log entry adds the file
	co      []index.CoChange
	owners  []index.AuthorStat
	monthly []float64
	lastMon time.Time // the month of monthly's last value
}

func newCard(r whyRef) *card {
	v := view{path: r.path, line: r.line, head: true, note: r.hint}
	if idx, ok := r.src.e.base(r.src.ix); ok {
		v.at = idx
	} else {
		v.at = r.src.ix.NumCommits() - 1
		v.head = false
	}
	return loadCard(r, v)
}

func loadCard(r whyRef, v view) *card {
	ix := r.src.ix
	c := &card{ref: r, v: v}
	anc := ix.Ancestors(v.at)
	scope := index.Scope(anc.Has)
	c.log = ix.Log(v.path, true, scope, 0)
	if len(c.log) > 0 {
		last := c.log[len(c.log)-1]
		for _, ch := range ix.Changes(last.Idx) {
			if ch.Path == last.Path && ch.Kind == 'A' {
				c.created = true
			}
		}
	}
	c.co = ix.CoChanges(v.path, 4)
	c.owners = ix.Owners(index.PathMatch{Path: v.path}, scope, 3)
	c.monthly, c.lastMon = monthly(ix, c.log)

	if !ix.HasHunks() {
		if v.line > 0 {
			c.err = "line-level blame needs line ranges: mx build --hunks (or pull --hunks)"
		}
		return c // a whole file's card needs no line ranges
	}
	n, err := ix.LineCount(v.path, v.at)
	if err != nil {
		c.err = err.Error()
		return c
	}
	c.lines = n
	if v.line == 0 {
		return c
	}
	if v.line > n {
		c.err = fmt.Sprintf("line %d is past the end of the file (%d lines at %s)", v.line, n, short(ix.Hash(v.at)))
		return c
	}
	bl, err := ix.Blame(v.path, v.at, n)
	if err != nil {
		c.err = err.Error()
		return c
	}
	b := bl[v.line-1]
	c.blame = &b
	c.commit = ix.Commit(b.Commit)
	c.text, c.textOK = r.src.lineText(v, n)
	return c
}

// lineText reads the line's text: from the working tree when it is HEAD's
// version, else from git's copy of the file, which a shallow clone may not
// have.
func (s *whySource) lineText(v view, n int) (string, bool) {
	var content []byte
	if v.head {
		if b, err := os.ReadFile(filepath.Join(s.r.root, filepath.FromSlash(v.path))); err == nil && len(splitLines(b)) == n {
			content = b
		}
	}
	if content == nil {
		out, err := exec.Command("git", "-C", s.r.root, "cat-file", "blob", s.ix.Hash(v.at).String()+":"+v.path).Output()
		if err != nil {
			return "", false
		}
		content = out
	}
	lines := splitLines(content)
	if v.line-1 < len(lines) {
		return strings.TrimRight(strings.ReplaceAll(lines[v.line-1], "\t", "    "), " \r"), true
	}
	return "", false
}

// monthly counts the commits in each month, from the file's first month
// (at most 24 months back, at least 6) to this month, and returns the last
// month counted.
func monthly(ix *index.Index, log []index.LogEntry) ([]float64, time.Time) {
	if len(log) == 0 {
		return nil, time.Time{}
	}
	end := ix.Commit(log[0].Idx).Time
	if now := time.Now(); now.After(end) {
		end = now
	}
	months := func(t time.Time) int { return t.Year()*12 + int(t.Month()) }
	span := months(end) - months(ix.Commit(log[len(log)-1].Idx).Time) + 1
	span = max(6, min(24, span))
	out := make([]float64, span)
	for _, l := range log {
		k := months(end) - months(ix.Commit(l.Idx).Time)
		if k >= 0 && k < span {
			out[span-1-k]++
		}
	}
	return out, end
}

// activity draws monthly commits as labeled bars, the busiest month in the
// accent, and says in words what the bars show.
func (c *card) activity() viz.Figure {
	n := len(c.monthly)
	labels, years := make([]string, n), make([]string, n)
	busiest, total := 0, 0.0
	for i, v := range c.monthly {
		m := time.Date(c.lastMon.Year(), c.lastMon.Month()-time.Month(n-1-i), 1, 0, 0, 0, 0, time.UTC)
		labels[i] = m.Format("Jan")[:1]
		if i == 0 || m.Month() == time.January {
			years[i] = m.Format("2006")
		}
		if v > c.monthly[busiest] {
			busiest = i
		}
		total += v
	}
	bm := time.Date(c.lastMon.Year(), c.lastMon.Month()-time.Month(n-1-busiest), 1, 0, 0, 0, 0, time.UTC)
	ix := c.ref.src.ix
	last := ix.Commit(c.log[0].Idx).Time
	commits := comma(int(total)) + " commits"
	if total == 1 {
		commits = "1 commit"
	}
	summary := fmt.Sprintf("%s in the last %d months, most in %s (%d). Last changed %s.",
		commits, n, bm.Format("January 2006"), int(c.monthly[busiest]), ago(last))
	if older := len(c.log) - int(total); older > 0 {
		summary += fmt.Sprintf(" %s older commits aren't shown.", comma(older))
	}
	return viz.Figure{Title: "commits per month", Blocks: []viz.Block{
		viz.Histogram{Values: c.monthly, Labels: labels, Under: years, Accent: busiest},
		viz.Blank{},
		viz.Para{Text: summary, Style: viz.Muted},
	}}
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 60*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month") + " ago"
	}
	return plural(int(math.Round(d.Hours()/24/365)), "year") + " ago"
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// figures draws the card: the line and the commit that last changed it,
// the file's history, and who and what it changes with.
func (c *card) figures() (left, right []viz.Figure) {
	ix := c.ref.src.ix
	title := c.v.path
	if c.v.line > 0 {
		title += ":" + strconv.Itoa(c.v.line)
	}
	var top []viz.Block
	if c.v.note != "" {
		top = append(top, viz.Para{Text: c.v.note, Style: viz.Muted}, viz.Blank{})
	}
	code := func(text string, ok bool, st viz.Style) viz.Span {
		switch {
		case !ok:
			return viz.S("(text not in this clone)", viz.Muted)
		case strings.TrimSpace(text) == "":
			return viz.S("(blank line)", viz.Muted)
		}
		return viz.S(strings.TrimLeft(text, " "), st)
	}
	if n := c.v.newer; n != nil {
		// An older version: show it over the version that replaced it, the
		// way a diff does.
		w := len(strconv.Itoa(max(c.v.line, n.line)))
		top = append(top, viz.Text{
			viz.M("before " + short(n.commit.Hash) + ", this line read:"),
			viz.L(viz.S(fmt.Sprintf("- %*d │ ", w, c.v.line), viz.Bad), code(c.text, c.textOK, viz.Bad)),
			viz.L(viz.S(fmt.Sprintf("+ %*d │ ", w, n.line), viz.Good), code(n.text, n.textOK, viz.Good)),
		}, viz.Blank{})
	} else if c.v.line > 0 && c.textOK {
		top = append(top, viz.Text{viz.L(viz.S(strconv.Itoa(c.v.line)+" │ ", viz.Muted), code(c.text, true, viz.Strong))}, viz.Blank{})
	}
	switch {
	case c.blame != nil:
		cm := c.commit
		label := "last changed in"
		if c.v.newer != nil {
			label = "this version was written in"
		}
		lines := []viz.Line{
			viz.M(label),
			viz.L(viz.S("● ", viz.Accent), viz.S(cm.Subject, viz.Accent)),
			viz.M("  " + short(cm.Hash) + " · " + name(cm.Author) + " · " + day(cm.Time) + " (" + ago(cm.Time) + ")"),
		}
		if c.blame.Path != c.v.path || int(c.blame.Line) != c.v.line {
			where := "line " + strconv.FormatUint(uint64(c.blame.Line), 10)
			if c.blame.Path != c.v.path {
				where += " of " + c.blame.Path
			}
			lines = append(lines, viz.M("  it was "+where+" in that commit"))
		}
		if c.blame.Boundary {
			lines = append(lines, viz.M("  the index's history starts here; the line may be older"))
		}
		top = append(top, viz.Text(lines))
	case c.err != "":
		top = append(top, viz.Text{viz.M(c.err)})
	default:
		first := ""
		if len(c.log) > 0 {
			cm := ix.Commit(c.log[len(c.log)-1].Idx)
			first = ", since " + day(cm.Time)
		}
		authors := map[string]bool{}
		for _, l := range c.log {
			authors[ix.Commit(l.Idx).Author] = true
		}
		stat := viz.Stat{
			{Value: len(c.log), Label: "commits" + first, Style: viz.Accent},
			{Value: len(authors), Label: "authors"},
		}
		if c.lines > 0 {
			stat = append(viz.Stat{{Value: c.lines, Label: "lines", Style: viz.Accent}}, stat...)
			stat[1].Style = viz.Plain
		}
		top = append(top, stat)
	}
	left = append(left, viz.Figure{Title: title, Blocks: top})

	// History: the newest commits, and the blamed one if it is older.
	var events viz.Timeline
	blamed := uint32(math.MaxUint32)
	if c.blame != nil {
		blamed = c.blame.Commit
	}
	pos := -1
	for i, l := range c.log {
		if l.Idx == blamed {
			pos = i
		}
	}
	add := func(i int) {
		l := c.log[i]
		cm := ix.Commit(l.Idx)
		ev := viz.Event{When: day(cm.Time), Text: cm.Subject, Note: name(cm.Author) + " · " + short(cm.Hash), Now: l.Idx == blamed}
		if l.Path != c.v.path {
			ev.Note += " · as " + l.Path
		}
		if i == len(c.log)-1 && c.created {
			ev.Open = true
			ev.Note += " · created"
		}
		events = append(events, ev)
	}
	const shown = 5
	for i := 0; i < len(c.log) && i < shown; i++ {
		add(i)
	}
	if pos >= shown {
		if pos > shown {
			events = append(events, viz.Event{Text: fmt.Sprintf("%d more", pos-shown), Gap: true})
		}
		add(pos)
	}
	hist := []viz.Block{events}
	if len(c.log) > len(events) {
		hist = append(hist, viz.Blank{}, viz.Text{viz.M(fmt.Sprintf("%d commits in all", len(c.log)))})
	}
	left = append(left, viz.Figure{Title: "history", Blocks: hist})

	if len(c.co) > 0 {
		var bars []viz.Bar
		for i, co := range c.co {
			st := viz.Plain
			if i == 0 {
				st = viz.Accent
			}
			bars = append(bars, viz.Bar{Label: co.Path, Value: co.Confidence, Text: fmt.Sprintf("%.0f%%", co.Confidence*100), Style: st})
		}
		right = append(right, viz.Figure{Title: "changes with", Blocks: []viz.Block{viz.Rank{Bars: bars, Max: 1}, viz.Blank{}, viz.Text{viz.M("share of this file's commits that also changed it")}}})
	}
	if len(c.owners) > 0 {
		var bars []viz.Bar
		for i, o := range c.owners {
			st := viz.Plain
			if i == 0 {
				st = viz.Accent
			}
			bars = append(bars, viz.Bar{Label: name(o.Author), Value: o.Score, Text: plural(o.Commits, "commit"), Style: st})
		}
		right = append(right, viz.Figure{Title: "who knows it", Blocks: []viz.Block{viz.Rank{Bars: bars}, viz.Blank{}, viz.Text{viz.M("weighted toward recent work")}}})
	}
	if c.monthly != nil {
		right = append(right, c.activity())
	}
	return left, right
}

// render lays the card out for a screen cols wide, filling it: in two
// columns when there is room, or one. With rows, figures that don't fit are
// left out, from the last.
func (c *card) render(cols, rows int, t float64, color bool) []string {
	left, right := c.figures()
	const gap = 2
	if cols >= 2*52+gap {
		w := (cols - gap) / 2
		l := viz.Render(left, w, t, color)
		r := viz.Render(fitFigures(right, w, rows), w, t, color)
		if rows > 0 && len(l) > rows {
			l = viz.Render(fitFigures(left, w, rows), w, t, color)
		}
		return viz.Columns(gap, []int{w, w}, l, r)
	}
	return viz.Render(fitFigures(append(left, right...), cols, rows), cols, t, color)
}

func fitFigures(figs []viz.Figure, width, rows int) []viz.Figure {
	for rows > 0 && len(figs) > 1 && len(viz.Render(figs, width, 1, false)) > rows {
		figs = figs[:len(figs)-1]
	}
	return figs
}

// dig returns the view one step back in the line's own history: the line
// it replaced in the commit that last changed it, as of that commit's
// parent. A line its commit added without replacing anything has no older
// version, and dig says so.
func (c *card) dig() (view, error) {
	if c.blame == nil {
		if c.err != "" {
			return view{}, errors.New(c.err)
		}
		return view{}, errors.New("pick a line to dig into")
	}
	ix := c.ref.src.ix
	b := c.blame
	who := short(c.commit.Hash)
	if b.Boundary {
		return view{}, errors.New("the index's history starts here; this line may be older")
	}
	prev, err := ix.Previous(b.Path, b.Commit, b.Line)
	switch {
	case err != nil:
		return view{}, err
	case prev.Created:
		return view{}, fmt.Errorf("this is the first version: the line came with the file, in %s", who)
	case prev.Added:
		return view{}, fmt.Errorf("this is the first version: %s added the line", who)
	case len(c.commit.Parents) == 0:
		return view{}, fmt.Errorf("%s is the first commit", who)
	}
	parent := c.commit.Parents[0]
	note := ""
	if prev.OldPath != c.v.path {
		note = "the file was " + prev.OldPath + " then"
	}
	newer := &newerVersion{text: c.text, textOK: c.textOK, line: c.v.line, commit: c.commit}
	return view{path: prev.OldPath, line: int(prev.OldLine), at: parent, note: note, newer: newer}, nil
}

type whyJSON struct {
	Path     string       `json:"path"`
	Line     int          `json:"line,omitempty"`
	Text     string       `json:"text,omitempty"`
	Commit   *commitJSON  `json:"commit,omitempty"`
	OrigPath string       `json:"orig_path,omitempty"`
	OrigLine uint32       `json:"orig_line,omitempty"`
	Boundary bool         `json:"boundary,omitempty"`
	Note     string       `json:"note,omitempty"`
	History  []commitJSON `json:"history"`
	Commits  int          `json:"commits"`
	CoChange []coJSON     `json:"cochange"`
	Owners   []ownerJSON  `json:"owners"`
	Monthly  []float64    `json:"monthly"`
}

type coJSON struct {
	Path       string  `json:"path"`
	Count      int     `json:"count"`
	Confidence float64 `json:"confidence"`
}

type ownerJSON struct {
	Author  string  `json:"author"`
	Commits int     `json:"commits"`
	Score   float64 `json:"score"`
}

func cardJSON(r whyRef) whyJSON {
	c := newCard(r)
	ix := r.src.ix
	cj := func(idx uint32, p string) commitJSON {
		row := commitRow(ix.Commit(idx))
		row.Path = p
		return row
	}
	out := whyJSON{Path: c.v.path, Line: c.v.line, Text: c.text, Note: c.err, Commits: len(c.log), Monthly: c.monthly,
		History: []commitJSON{}, CoChange: []coJSON{}, Owners: []ownerJSON{}}
	if c.blame != nil {
		x := cj(c.blame.Commit, "")
		out.Commit, out.OrigPath, out.OrigLine, out.Boundary = &x, c.blame.Path, c.blame.Line, c.blame.Boundary
	}
	for i, l := range c.log {
		if i == 10 {
			break
		}
		out.History = append(out.History, cj(l.Idx, l.Path))
	}
	for _, co := range c.co {
		out.CoChange = append(out.CoChange, coJSON{co.Path, co.Count, co.Confidence})
	}
	for _, o := range c.owners {
		out.Owners = append(out.Owners, ownerJSON{o.Author, o.Commits, o.Score})
	}
	return out
}
