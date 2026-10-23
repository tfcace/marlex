// Package viz draws figures out of text, in the style of mdxcn
// (https://mdxcn.dev): a dashed frame with + corners and the title as
// [ TITLE ], charts made of characters, one accent color for the row to
// read, and muted text for asides. Motion is a single reveal of about a
// fifth of a second: bars grow, timelines and sparklines draw in, text
// fades from muted. It never loops or pulses, and without a terminal the
// figure prints once, finished.
package viz

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Style is how a span is drawn. There is one accent; Bad and Good are for
// test results only.
type Style uint8

const (
	Plain  Style = iota
	Accent       // now, chosen, the row to read
	Muted        // an aside, older, a caption
	Strong       // emphasis without color
	Bad
	Good
)

var sgr = [...]string{
	Plain:  "",
	Accent: "\x1b[38;5;215m",
	Muted:  "\x1b[2m",
	Strong: "\x1b[1m",
	Bad:    "\x1b[31m",
	Good:   "\x1b[32m",
}

type Span struct {
	Text  string
	Style Style
}

// Line is one row of a figure's body.
type Line []Span

func S(text string, st Style) Span { return Span{text, st} }

// L builds a line from spans.
func L(spans ...Span) Line { return Line(spans) }

// T is a line of plain text.
func T(text string) Line { return Line{{Text: text}} }

// M is a line of muted text.
func M(text string) Line { return Line{{text, Muted}} }

func (l Line) width() int {
	n := 0
	for _, s := range l {
		n += Width(s.Text)
	}
	return n
}

// Block is part of a figure's body, drawn at time t of the reveal, from 0
// to 1. A block returns the same number of lines for every t, so a frame
// can be redrawn in place.
type Block interface {
	Draw(width int, t float64) []Line
}

// Figure is one framed figure.
type Figure struct {
	Title  string
	Blocks []Block
}

// Render draws the figure width cells wide at time t.
func (f Figure) Render(width int, t float64, color bool) []string {
	inner := width - 4
	var body []Line
	body = append(body, nil)
	for _, b := range f.Blocks {
		body = append(body, b.Draw(inner, t)...)
	}
	body = append(body, nil)

	out := make([]string, 0, len(body)+2)
	out = append(out, top(f.Title, width, color))
	edge := paint("|", Muted, color)
	for _, l := range body {
		l = Fit(l, inner)
		out = append(out, edge+" "+l.String(color)+strings.Repeat(" ", inner-l.width())+" "+edge)
	}
	out = append(out, paint("+"+strings.Repeat("-", width-2)+"+", Muted, color))
	return out
}

func top(title string, width int, color bool) string {
	if title == "" {
		return paint("+"+strings.Repeat("-", width-2)+"+", Muted, color)
	}
	title = truncate(strings.ToUpper(title), width-10)
	label := "[ " + title + " ]"
	dashes := width - 2 - Width(label) - 2
	left := dashes / 2
	right := dashes - left
	return paint("+"+strings.Repeat("-", left)+" ", Muted, color) +
		paint(label, Strong, color) +
		paint(" "+strings.Repeat("-", right)+"+", Muted, color)
}

func paint(s string, st Style, color bool) string {
	if !color || sgr[st] == "" || s == "" {
		return s
	}
	return sgr[st] + s + "\x1b[0m"
}

// String renders a line with or without color.
func (l Line) String(color bool) string {
	var b strings.Builder
	for _, s := range l {
		b.WriteString(paint(s.Text, s.Style, color))
	}
	return b.String()
}

// Fit cuts a line to width cells, ending it with … when it was cut.
func Fit(l Line, width int) Line {
	if l.width() <= width {
		return l
	}
	var out Line
	left := width - 1
	for _, s := range l {
		w := Width(s.Text)
		if w <= left {
			out = append(out, s)
			left -= w
			continue
		}
		out = append(out, Span{truncateTo(s.Text, left) + "…", s.Style})
		break
	}
	return out
}

func truncate(s string, width int) string {
	if Width(s) <= width {
		return s
	}
	return truncateTo(s, width-1) + "…"
}

func truncateTo(s string, width int) string {
	var b strings.Builder
	for _, r := range s {
		w := runeWidth(r)
		if w > width {
			break
		}
		width -= w
		b.WriteRune(r)
	}
	return b.String()
}

// Width is how many terminal cells s takes.
func Width(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

func runeWidth(r rune) int {
	switch {
	case r == utf8.RuneError || r < 0x20 || unicode.Is(unicode.Mn, r) || r == 0x200d || (r >= 0xfe00 && r <= 0xfe0f):
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3, r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1faff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// ease is ease-out cubic: fast start, gentle landing.
func ease(t float64) float64 {
	t = clamp(t)
	return 1 - math.Pow(1-t, 3)
}

func clamp(t float64) float64 { return math.Max(0, math.Min(1, t)) }

// stagger is the time row i of n starts and finishes its own reveal, so
// rows land one after another inside the figure's reveal.
func stagger(t float64, i, n int) float64 {
	if n <= 1 {
		return ease(t)
	}
	const spread = 0.45
	start := spread * float64(i) / float64(n-1)
	return ease((t - start) / (1 - spread))
}

// Render draws figures one under another, each starting its reveal a
// little after the one above.
func Render(figs []Figure, width int, t float64, color bool) []string {
	var out []string
	for i, f := range figs {
		ft := t
		if len(figs) > 1 {
			ft = clamp((t - 0.25*float64(i)/float64(len(figs)-1)) / 0.75)
		}
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, f.Render(width, ft, color)...)
	}
	return out
}

// Duration is how long the reveal takes.
const Duration = 320 * time.Millisecond

const frame = 16 * time.Millisecond

// Play writes the figures below the cursor. With motion, it draws the
// reveal in place and leaves the finished figures on screen; without, it
// writes them finished.
func Play(w io.Writer, figs []Figure, width int, motion, color bool) {
	Animate(w, func(t float64) []string { return Render(figs, width, t, color) }, motion, false)
}

// Animate draws render(t) for t from 0 to 1. Every frame must have the same
// number of lines. With home, frames are drawn from the top-left corner of
// the screen (for a full-screen view); otherwise they are redrawn in place
// below the cursor, which must have room for them.
func Animate(w io.Writer, render func(t float64) []string, motion, home bool) {
	n := int(Duration / frame)
	if !motion {
		n = 0
	}
	if motion {
		io.WriteString(w, "\x1b[?25l") // hide the cursor while drawing
		defer io.WriteString(w, "\x1b[?25h")
	}
	var prev int
	for i := 0; i <= n; i++ {
		t := 1.0
		if n > 0 {
			t = float64(i) / float64(n)
		}
		lines := render(t)
		var b strings.Builder
		switch {
		case home:
			b.WriteString("\x1b[H")
		case prev > 0:
			fmt.Fprintf(&b, "\r\x1b[%dA", prev)
		}
		for j, l := range lines {
			if motion || home {
				b.WriteString("\x1b[2K")
			}
			b.WriteString(l)
			switch {
			case !home:
				b.WriteString("\n")
			case j < len(lines)-1:
				b.WriteString("\r\n") // a full-screen view runs with output processing off
			}
		}
		if home {
			b.WriteString("\x1b[J") // clear what an earlier, taller view left
		}
		io.WriteString(w, b.String())
		prev = len(lines)
		if i < n {
			time.Sleep(frame)
		}
	}
}

// Columns sets blocks of lines side by side. Each block's lines are
// padded to its width, so a short block leaves blank space under it.
func Columns(gap int, widths []int, blocks ...[]string) []string {
	n := 0
	for _, b := range blocks {
		n = max(n, len(b))
	}
	out := make([]string, n)
	for i := range out {
		var sb strings.Builder
		for k, b := range blocks {
			if k > 0 {
				sb.WriteString(strings.Repeat(" ", gap))
			}
			l := ""
			if i < len(b) {
				l = b[i]
			}
			sb.WriteString(l)
			if k < len(blocks)-1 {
				if w := Width(ansiRe.ReplaceAllString(l, "")); w < widths[k] {
					sb.WriteString(strings.Repeat(" ", widths[k]-w))
				}
			}
		}
		out[i] = sb.String()
	}
	return out
}

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
