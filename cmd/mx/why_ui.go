package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/tfcace/marlex/internal/index"
	"github.com/tfcace/marlex/internal/rex"
	"github.com/tfcace/marlex/internal/screen"
	"github.com/tfcace/marlex/internal/viz"
)

// whyUI is `mx why -i`, the view a terminal integration opens in a
// floating pane: one reference at a time, with keys to dig into the line's
// older versions and to move between the references found on screen.
type whyUI struct {
	e     env
	load  func() ([]whyRef, error) // finds the references again, after a build
	refs  []whyRef
	err   error
	cur   int     // which reference
	stack []*card // the card shown is the last; digging pushes
	files bool    // show the commit's files instead of the card
	flash string  // a one-time message in the footer
	out   io.Writer
}

// screenState is one look at a terminal: what watch.lua streams.
type screenState struct {
	Text    string `json:"text"`
	Row     int    `json:"row"`
	Col     int    `json:"col"`
	Rows    int    `json:"rows"`
	Cols    int    `json:"cols"`
	Program string `json:"program"`
	Pid     int    `json:"pid"`
	Title   string `json:"title"`
}

// editor is the editor showing this screen, if any: the foreground
// program, or one running under it.
func (st screenState) editor() editorProc {
	if ed := editorUnder(st.Pid); ed.name != "" {
		return ed
	}
	if p := filepath.Base(st.Program); screen.Editors[p] {
		return editorProc{name: p}
	}
	return editorProc{}
}

// following is the terminal a view follows, and how to find references on
// a new look at it.
type following struct {
	block, session string
	load           func(screenState) func() ([]whyRef, error)
}

func runWhyUI(e env, load func() ([]whyRef, error), fol *following, refs []whyRef, loadErr error) error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("why -i needs a terminal: %v", err)
	}
	defer term.Restore(fd, old)
	ui := &whyUI{e: e, load: load, refs: refs, err: loadErr, out: os.Stdout}
	io.WriteString(ui.out, "\x1b[?1049h\x1b[?25l") // alternate screen, no cursor
	defer io.WriteString(ui.out, "\x1b[?25h\x1b[?1049l")

	keys := make(chan string)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			keys <- string(buf[:n])
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	if ui.err == nil {
		ui.stack = []*card{newCard(refs[0])}
	}
	ui.draw(true)

	// Following an editor: each look at its screen that moves its file or
	// cursor line is re-blamed once the cursor has rested for a moment, so
	// holding j doesn't redraw for every line it passes.
	var screens <-chan screenState
	if fol != nil {
		var stop func()
		screens, stop = watchScreen(fol.block, fol.session)
		defer stop()
	}
	var pending *screenState
	var settle <-chan time.Time
	followed := ""
	if len(refs) > 0 {
		followed = refKey(refs[0])
	}
	for {
		select {
		case st, ok := <-screens:
			if !ok {
				screens = nil
				continue
			}
			pending = &st
			settle = time.After(150 * time.Millisecond)
		case <-settle:
			settle = nil
			if pending.editor().name == "" {
				continue // only an editor's cursor says what the reader looks at
			}
			load := fol.load(*pending)
			refs, err := load()
			if err != nil || refKey(refs[0]) == followed {
				continue
			}
			followed = refKey(refs[0])
			ui.load, ui.refs, ui.err = load, refs, nil
			ui.cur, ui.files = 0, false
			ui.stack = []*card{newCard(refs[0])}
			ui.draw(false)
		case <-winch:
			ui.draw(false)
		case k, ok := <-keys:
			if !ok || !ui.key(k) {
				return nil
			}
		}
	}
}

// key handles one key press and reports whether to keep going.
func (ui *whyUI) key(k string) bool {
	switch k {
	case "q", "\x03", "\x1b":
		if ui.files && k == "\x1b" {
			ui.files = false
			ui.draw(false)
			return true
		}
		return false
	}
	if ui.err != nil {
		if ni, ok := ui.buildable(); ok && k == "b" {
			ui.build(ni)
		}
		return true
	}
	c := ui.stack[len(ui.stack)-1]
	switch k {
	case "d", "j", "\x1b[B", "\x1bOB":
		v, err := c.dig()
		if err != nil {
			ui.flash = err.Error()
			ui.draw(false)
			return true
		}
		ui.stack = append(ui.stack, loadCard(c.ref, v))
		ui.files = false
		ui.draw(true)
	case "u", "k", "\x1b[A", "\x1bOA":
		if len(ui.stack) == 1 {
			ui.flash = "this is the current version"
			ui.draw(false)
			return true
		}
		ui.stack = ui.stack[:len(ui.stack)-1]
		ui.files = false
		ui.draw(true)
	case "n", "l", "\t", "\x1b[C", "\x1bOC":
		ui.move(1)
	case "p", "h", "\x1b[Z", "\x1b[D", "\x1bOD":
		ui.move(-1)
	case "s", "\r":
		if c.blame == nil {
			ui.flash = "no commit to show"
			ui.draw(false)
			return true
		}
		ui.files = !ui.files
		ui.draw(true)
	case "y":
		if c.blame == nil {
			ui.flash = "no commit to copy"
		} else {
			h := c.commit.Hash.String()
			// OSC 52 puts text on the clipboard, through the terminal.
			fmt.Fprintf(ui.out, "\x1b]52;c;%s\x1b\\", base64.StdEncoding.EncodeToString([]byte(h)))
			ui.flash = "copied " + h
		}
		ui.draw(false)
	}
	return true
}

func (ui *whyUI) move(d int) {
	if len(ui.refs) < 2 {
		ui.flash = "no other files on screen"
		ui.draw(false)
		return
	}
	ui.cur = (ui.cur + d + len(ui.refs)) % len(ui.refs)
	ui.stack = []*card{newCard(ui.refs[ui.cur])}
	ui.files = false
	ui.draw(true)
}

func (ui *whyUI) draw(animate bool) {
	cols, rows := termSize()
	color := os.Getenv("NO_COLOR") == ""
	flash := ui.flash
	ui.flash = ""
	frame := func(t float64) []string {
		var body []string
		header := "mx why"
		switch {
		case ui.err != nil:
			title := "nothing to show"
			if _, ok := ui.err.(needsIndex); ok {
				title = "no index yet"
			}
			body = viz.Render([]viz.Figure{{Title: title, Blocks: []viz.Block{viz.Para{Text: ui.err.Error()}}}}, cols, t, color)
		default:
			c := ui.stack[len(ui.stack)-1]
			r := ui.refs[ui.cur]
			if len(ui.refs) > 1 {
				header += fmt.Sprintf(" · %d of %d on screen", ui.cur+1, len(ui.refs))
			}
			if r.raw != c.v.path && r.raw != fmt.Sprintf("%s:%d", c.v.path, c.v.line) && !filepath.IsAbs(r.raw) {
				header += " · " + r.raw
			}
			if len(ui.stack) > 1 {
				header += fmt.Sprintf(" · %s back", plural(len(ui.stack)-1, "version"))
			}
			if ui.files {
				body = c.filesFigure(cols, rows-3, t, color)
			} else {
				body = c.render(cols, rows-3, t, color)
			}
		}
		lines := []string{viz.Fit(viz.L(viz.S(header, viz.Muted)), cols).String(color)}
		lines = append(lines, body...)
		if len(lines) > rows-1 {
			lines = lines[:rows-1]
		}
		for len(lines) < rows-1 {
			lines = append(lines, "")
		}
		var foot viz.Line
		if flash != "" {
			foot = viz.L(viz.S(flash, viz.Accent))
		} else if _, ok := ui.buildable(); ok {
			foot = viz.Keys{{"b", "build it now"}, {"q", "close"}}.Draw(cols, 1)[0]
		} else if ui.err != nil {
			foot = viz.Keys{{"q", "close"}}.Draw(cols, 1)[0]
		} else {
			keys := viz.Keys{{"↓", "previous version"}}
			if len(ui.stack) > 1 {
				keys = append(keys, [2]string{"↑", "newer version"})
			}
			if len(ui.refs) > 1 {
				keys = append(keys, [2]string{"←→", "other files"})
			}
			keys = append(keys, viz.Keys{{"s", "commit"}, {"y", "copy hash"}, {"q", "close"}}...)
			foot = keys.Draw(cols, 1)[0]
		}
		return append(lines, viz.Fit(foot, cols).String(color))
	}
	viz.Animate(ui.out, frame, animate && os.Getenv("MX_NO_MOTION") == "", true)
}

// filesFigure shows the blamed commit: its message and the files it changed.
func (c *card) filesFigure(cols, rows int, t float64, color bool) []string {
	ix := c.ref.src.ix
	cm := c.commit
	var files []viz.Line
	changes := ix.Changes(c.blame.Commit)
	for i, ch := range changes {
		if i == max(rows-12, 3) && len(changes) > i+1 {
			files = append(files, viz.M(fmt.Sprintf("… and %d more", len(changes)-i)))
			break
		}
		st := viz.Plain
		if ch.Path == c.blame.Path {
			st = viz.Accent
		}
		p := ch.Path
		if ch.OldPath != "" && ch.OldPath != ch.Path {
			p = ch.OldPath + " → " + ch.Path
		}
		files = append(files, viz.L(viz.S(string(ch.Kind)+"  ", viz.Muted), viz.S(p, st)))
	}
	w := cols
	fig := viz.Figure{Title: "commit", Blocks: []viz.Block{
		viz.Text{viz.L(viz.S(cm.Subject, viz.Strong)), viz.M(name(cm.Author) + " · " + day(cm.Time) + " · " + ago(cm.Time)), viz.M(cm.Hash.String())},
		viz.Blank{},
		viz.Text(files),
	}}
	return viz.Render([]viz.Figure{fig}, w, t, color)
}

// buildable reports whether the view is stuck on a repository without an
// index that it can build: not a shallow clone, which needs a pull instead.
func (ui *whyUI) buildable() (needsIndex, bool) {
	ni, ok := ui.err.(needsIndex)
	return ni, ok && !ni.shallow
}

// build indexes the repository, with line ranges for blame, showing the
// meter in the view, then looks for the references again.
func (ui *whyUI) build(ni needsIndex) {
	cols, rows := termSize()
	color := os.Getenv("NO_COLOR") == ""
	w := cols
	var last time.Time
	caption := "listing commits"
	show := func(done, total int, force bool) {
		if !force && time.Since(last) < 50*time.Millisecond {
			return
		}
		last = time.Now()
		blocks := []viz.Block{}
		if total > 0 {
			blocks = append(blocks, viz.Meter{Value: float64(done) / float64(total), Text: fmt.Sprintf("%s of %s commits", comma(done), comma(total))})
		}
		blocks = append(blocks, viz.Text{viz.M(caption)})
		lines := viz.Render([]viz.Figure{{Title: "indexing " + filepath.Base(ni.root), Blocks: blocks}}, w, 1, color)
		viz.Animate(ui.out, func(float64) []string { return padTo(lines, rows) }, false, true)
	}
	show(0, 0, true)
	dir, err := ni.e.indexDir()
	if err == nil {
		t0 := time.Now()
		var res *index.BuildResult
		res, err = index.Build(ni.root, dir, index.BuildOptions{
			Hunks:    true,
			Progress: func(done, total int) { show(done, total, false) },
			Log:      writerFunc(func(p []byte) { caption = strings.TrimSpace(string(p)) }),
		})
		if err == nil {
			ui.flash = fmt.Sprintf("indexed %s commits in %s", comma(res.NewCommits), time.Since(t0).Round(100*time.Millisecond))
		}
	}
	if err != nil {
		ui.err = fmt.Errorf("building the index failed: %v", err)
		ui.draw(false)
		return
	}
	ui.refs, ui.err = ui.load()
	ui.cur, ui.files = 0, false
	if ui.err == nil {
		ui.stack = []*card{newCard(ui.refs[0])}
	}
	ui.draw(true)
}

func padTo(lines []string, rows int) []string {
	for len(lines) < rows {
		lines = append(lines, "")
	}
	return lines
}

// writerFunc adapts a function to io.Writer.
type writerFunc func(p []byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

func refKey(r whyRef) string { return fmt.Sprintf("%s:%s:%d", r.src.r.root, r.path, r.line) }

// watchScreen runs watch.lua with rex do against block and streams what
// it sees. The channel closes when the block does; stop ends the watcher.
func watchScreen(block, session string) (<-chan screenState, func()) {
	out := make(chan screenState)
	rexBin, err := exec.LookPath("rex")
	if err != nil && os.Getenv("REX_BIN_DIR") != "" {
		rexBin, err = filepath.Join(os.Getenv("REX_BIN_DIR"), "rex"), nil
	}
	if err != nil {
		close(out)
		return out, func() {}
	}
	args := []string{"do", "-", "block=" + block}
	if session != "" {
		args = []string{"do", "-s", session, "-", "block=" + block}
	}
	cmd := exec.Command(rexBin, args...)
	cmd.Stdin = strings.NewReader(rex.Watch)
	pipe, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		close(out)
		return out, func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(out)
		sc := bufio.NewScanner(pipe)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			var st screenState
			if json.Unmarshal(sc.Bytes(), &st) != nil {
				continue
			}
			select {
			case out <- st:
			case <-done:
				return
			}
		}
		cmd.Wait()
	}()
	return out, func() {
		close(done)
		cmd.Process.Kill()
	}
}
