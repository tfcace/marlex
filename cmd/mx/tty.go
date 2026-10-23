package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/tfcace/marlex/internal/screen"
)

// termSize is the terminal's size, or 80x24 when stdout is not one.
func termSize() (cols, rows int) {
	if c, r, err := term.GetSize(int(os.Stdout.Fd())); err == nil && c > 0 {
		return c, r
	}
	cols, _ = strconv.Atoi(os.Getenv("COLUMNS"))
	rows, _ = strconv.Atoi(os.Getenv("LINES"))
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return cols, rows
}

func stdoutTTY() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// colorOn reports whether to color output: on a terminal, unless NO_COLOR
// is set.
func colorOn() bool {
	return stdoutTTY() && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// motionOn reports whether figures animate: on a color terminal, unless
// MX_NO_MOTION is set.
func motionOn() bool { return colorOn() && os.Getenv("MX_NO_MOTION") == "" }

// editorProc is an editor process: its name, and its pid when known.
type editorProc struct {
	name string
	pid  int
}

// editorUnder finds a known editor among pid and its descendants, nearest
// first. Rex names the terminal's foreground process, but under a login
// shell that is /usr/bin/login, and a shell such as hash runs commands on a
// pty of its own, so the editor is found by walking down from there.
func editorUnder(pid int) editorProc {
	if pid <= 0 {
		return editorProc{}
	}
	out, err := exec.Command("ps", "-ax", "-o", "pid=,ppid=,comm=").Output()
	if err != nil {
		return editorProc{}
	}
	children := map[int][]int{}
	names := map[int]string{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		p, _ := strconv.Atoi(f[0])
		pp, _ := strconv.Atoi(f[1])
		children[pp] = append(children[pp], p)
		names[p] = filepath.Base(strings.Join(f[2:], " "))
	}
	level := []int{pid}
	for depth := 0; depth < 8 && len(level) > 0; depth++ {
		var next []int
		for _, p := range level {
			if screen.Editors[names[p]] {
				return editorProc{names[p], p}
			}
			next = append(next, children[p]...)
		}
		level = next
	}
	return editorProc{}
}
