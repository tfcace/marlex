// Package screen finds references to files, and to lines in them, in the
// text of a terminal screen: compiler errors, test failures, stack traces,
// grep output, diffs, an agent's transcript. Whether a reference names a
// real file is for the caller to decide; this package is deliberately
// generous and leaves that to the index.
package screen

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Ref is one reference found on screen.
type Ref struct {
	Path string // as written, e.g. ./internal/x.go or a/README.md
	Line int    // 0 when no line was given
	Row  int    // 0-based screen row where the reference starts
	Col  int    // 0-based column, in bytes of the row
}

var patterns = []*regexp.Regexp{
	// Python: File "pkg/mod.py", line 12
	regexp.MustCompile(`File "([^"]+)", line (\d+)`),
	// tsc and MSBuild: src/a.ts(12,5)
	regexp.MustCompile(`([\w./@+~-]+\.\w+)\((\d+),\d+\)`),
	// GitHub links: path/to/x.go#L42
	regexp.MustCompile(`([\w./@+~-]+\.\w+)#L(\d+)`),
	// The common form: path:line(:col), or a bare path with a slash or an
	// extension.
	regexp.MustCompile(`([\w./@+~-]*[\w@+~-](?:\.\w+|/[\w.@+~-]+))(?::(\d+))?`),
}

// Find returns the references in lines, a screen's rows. Rows exactly cols
// wide are treated as wrapped onto the next row, so a path cut by the
// right edge is found whole; cols of 0 turns that off.
func Find(lines []string, cols int) []Ref {
	var out []Ref
	seen := map[[3]int]bool{} // row, col, end: the first pattern to claim a span keeps it
	for row := 0; row < len(lines); row++ {
		text, start := lines[row], row
		for cols > 0 && len([]rune(lines[row])) == cols && row+1 < len(lines) {
			row++
			text += lines[row]
		}
		var claimed [][2]int
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
				if overlaps(claimed, m[0], m[1]) {
					continue
				}
				p := text[m[2]:m[3]]
				line := len(m) > 4 && m[4] >= 0
				if skip(text, m[2], p, line) {
					continue
				}
				claimed = append(claimed, [2]int{m[0], m[1]})
				r := Ref{Path: strings.TrimRight(p, "."), Row: start, Col: m[2]}
				if line {
					r.Line, _ = strconv.Atoi(text[m[4]:m[5]])
				}
				if cols > 0 && r.Col >= cols {
					r.Row += r.Col / cols
					r.Col %= cols
				}
				key := [3]int{start, m[2], m[3]}
				if !seen[key] {
					seen[key] = true
					out = append(out, r)
				}
			}
		}
	}
	return out
}

func overlaps(spans [][2]int, a, b int) bool {
	for _, s := range spans {
		if a < s[1] && s[0] < b {
			return true
		}
	}
	return false
}

var bareFile = regexp.MustCompile(`^[A-Za-z_.@][\w.@+~-]*\.[a-z][a-z0-9]{0,7}$`)

// skip drops what is clearly not a path: URLs, version numbers, durations,
// and Go selectors such as io.Writer. A reference with neither a slash nor
// a line number must look like name.ext.
func skip(text string, at int, p string, line bool) bool {
	if strings.HasPrefix(p, "//") || strings.HasSuffix(text[:at], "://") {
		return true
	}
	if !line && !strings.Contains(p, "/") && !bareFile.MatchString(p) {
		return true
	}
	letters := false
	for _, r := range p {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			letters = true
			break
		}
	}
	return !letters || p == "." || p == ".."
}

// ByDistance orders refs by how close they are to the cursor: first the
// ones on or above its row, nearest first (the output that just printed),
// then the ones below it. Duplicate path and line pairs keep the closest.
func ByDistance(refs []Ref, row, col int) []Ref {
	dist := func(r Ref) int {
		if r.Row <= row {
			d := (row - r.Row) * 10000
			if r.Row == row {
				d += abs(col - r.Col)
			}
			return d
		}
		return 1<<30 + (r.Row-row)*10000 + r.Col
	}
	out := append([]Ref(nil), refs...)
	sort.SliceStable(out, func(i, j int) bool { return dist(out[i]) < dist(out[j]) })
	seen := map[string]bool{}
	k := 0
	for _, r := range out {
		key := r.Path + ":" + strconv.Itoa(r.Line)
		if !seen[key] {
			seen[key] = true
			out[k] = r
			k++
		}
	}
	return out[:k]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Editors are the programs Editor understands, by process name: editors,
// and less, which shows a file without a cursor.
var Editors = map[string]bool{
	"hx": true, "helix": true, "vim": true, "nvim": true, "vi": true, "view": true,
	"kak": true, "micro": true, "emacs": true, "nano": true, "less": true,
}

var (
	// Cursor positions in an editor's status line, on the row that names
	// the file: helix and kakoune 120:5, micro (120,5), emacs L120.
	namedPos = []*regexp.Regexp{
		regexp.MustCompile(`(?:^|\s)(\d+):\d+(?:\s|$)`),
		regexp.MustCompile(`\((\d+),\d+\)`),
		regexp.MustCompile(`(?:^|\s)L(\d+)(?:\s|$)`),
	}
	// Positions on rows of their own: vim's ruler, 120,5 followed by Top,
	// Bot, All or a percentage, and nano's constantshow, [ line 120/2275 ].
	ownPos = []*regexp.Regexp{
		regexp.MustCompile(`(?:^|\s)(\d+),\d+(?:-\d+)?\s+(?:Top|Bot|All|\d+%)\s*$`),
		regexp.MustCompile(`\[ line (\d+)/\d+`),
	}
	gutter = regexp.MustCompile(`^\s*(\d+)[\s│|▏]`)
	// less -M: "cmd/hash/main.go lines 120-168/320 37%".
	lessPrompt = regexp.MustCompile(`^:?\s*(\S+) lines (\d+)-\d+`)
	// nano's title bar: "  GNU nano 8.2     internal/shell/shell.go    Modified".
	nanoBar = regexp.MustCompile(`^\s*GNU nano [\d.]+\s+(\S+)`)
	// vim's default title with 'title' set: "shell.go (~/src/hash/internal) - VIM",
	// with + or - flags after the name.
	vimTitle = regexp.MustCompile(`^(\S+)(?: [-+=]+)? \((.+)\) - N?VIM\d*$`)
)

// Editor finds the file and line an editor shows. The terminal's title
// comes first, as it is set for exactly this (vim with
// titlestring=%F:%l). Then the screen: less's -M prompt (whose line is the
// top one shown, as less has no cursor), the file named in a status line at
// the bottom or a title bar at the top (nano), and the cursor's line from a
// position on screen (helix's 120:5, vim's ruler, nano's constantshow) or
// the gutter on the cursor's row. home expands ~ in vim's default title.
func Editor(lines []string, cursorRow int, title, home string) (Ref, bool) {
	var file Ref
	if t, ok := titleRef(title, home); ok {
		if t.Line > 0 {
			t.Row = cursorRow
			return t, true
		}
		file = t
	}
	n := len(lines)
	for row := n - 1; row >= max(0, n-2); row-- {
		if m := lessPrompt.FindStringSubmatch(lines[row]); m != nil {
			l, _ := strconv.Atoi(m[2])
			return Ref{Path: m[1], Line: l, Row: cursorRow}, true
		}
	}
	line := 0
	var rows []int
	for row := n - 1; row >= max(0, n-4); row-- {
		rows = append(rows, row)
	}
	if n > 0 {
		rows = append(rows, 0)
	}
	for _, row := range rows {
		name := ""
		if m := nanoBar.FindStringSubmatch(lines[row]); m != nil {
			name = m[1]
		} else {
			for _, r := range Find(lines[row:row+1], 0) {
				if r.Line == 0 {
					name = r.Path
					break
				}
			}
		}
		if name == "" {
			continue
		}
		if file.Path == "" {
			file.Path = name
		}
		for _, re := range namedPos {
			if m := re.FindStringSubmatch(lines[row]); m != nil {
				line, _ = strconv.Atoi(m[1])
				break
			}
		}
		break
	}
	if file.Path == "" {
		return Ref{}, false
	}
	for row := n - 1; line == 0 && row >= max(0, n-4); row-- {
		for _, re := range ownPos {
			if m := re.FindStringSubmatch(lines[row]); m != nil {
				line, _ = strconv.Atoi(m[1])
				break
			}
		}
	}
	if line == 0 && cursorRow >= 0 && cursorRow < n {
		if m := gutter.FindStringSubmatch(lines[cursorRow]); m != nil {
			line, _ = strconv.Atoi(m[1])
		}
	}
	return Ref{Path: file.Path, Line: line, Row: cursorRow}, true
}

// titleRef reads a file, and maybe a line, from a terminal title: path:line
// (vim's titlestring=%F:%l) or vim's default "name (dir) - VIM".
func titleRef(title, home string) (Ref, bool) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Ref{}, false
	}
	if m := vimTitle.FindStringSubmatch(title); m != nil {
		dir := m[2]
		if rest, ok := strings.CutPrefix(dir, "~"); ok && home != "" {
			dir = home + rest
		}
		return Ref{Path: strings.TrimSuffix(dir, "/") + "/" + m[1]}, true
	}
	for _, r := range Find([]string{title}, 0) {
		if r.Line > 0 {
			return r, true
		}
	}
	return Ref{}, false
}
