package viz

import (
	"fmt"
	"math"
	"strings"
)

// Text is lines that fade in: muted for the first part of the reveal.
type Text []Line

func (x Text) Draw(width int, t float64) []Line {
	if t >= 0.4 {
		return x
	}
	out := make([]Line, len(x))
	for i, l := range x {
		for _, s := range l {
			if s.Style != Bad && s.Style != Good {
				s.Style = Muted
			}
			out[i] = append(out[i], s)
		}
	}
	return out
}

// Blank is an empty line.
type Blank struct{}

func (Blank) Draw(int, float64) []Line { return []Line{nil} }

// Event is one point on a timeline.
type Event struct {
	When string
	Text string
	Note string // a muted line under the event
	Now  bool   // the event to read: accent and a filled mark
	Open bool   // an event that hasn't happened, or the end of what is known: ○
	Gap  bool   // events left out: a dotted rule and muted text
}

// Timeline lists events top to bottom, joined by a rule. Events appear one
// after another.
type Timeline []Event

func (tl Timeline) Draw(width int, t float64) []Line {
	whenW := 0
	for _, e := range tl {
		whenW = max(whenW, Width(e.When))
	}
	var out []Line
	for i, e := range tl {
		shown := stagger(t, i, len(tl)) > 0.15
		mark, st := "●", Plain
		if e.Open {
			mark = "○"
		}
		if e.Now {
			st = Accent
		}
		if e.Gap {
			mark, st = "┆", Muted
		}
		if !shown {
			out = append(out, nil)
		} else {
			fresh := stagger(t, i, len(tl)) < 0.6
			text := st
			if fresh && !e.Now {
				text = Muted
			}
			out = append(out, L(S(mark, st), S("  "+pad(e.When, whenW)+"  ", Muted), S(e.Text, text)))
		}
		last := i == len(tl)-1
		rule := "│"
		if last {
			rule = " "
		}
		switch {
		case e.Note != "" && shown:
			out = append(out, L(S(rule, Muted), S("  "+strings.Repeat(" ", whenW)+"  "+e.Note, Muted)))
		case e.Note != "":
			out = append(out, nil)
		case !last && shown:
			out = append(out, L(S(rule, Muted)))
		case !last:
			out = append(out, nil)
		}
	}
	return out
}

// Bar is one row of a Rank.
type Bar struct {
	Label string
	Value float64
	Text  string // printed after the bar, e.g. a count or a percentage
	Style Style  // the label's style; Accent marks the row to read
}

// Rank is labelled bars, [=====-----], longest first. Bars grow one after
// another; Max of 0 scales to the largest value.
type Rank struct {
	Bars []Bar
	Max  float64
}

func (r Rank) Draw(width int, t float64) []Line {
	if len(r.Bars) == 0 {
		return nil
	}
	labelW, textW, top := 0, 0, r.Max
	for _, b := range r.Bars {
		labelW = max(labelW, Width(b.Label))
		textW = max(textW, Width(b.Text))
		top = math.Max(top, b.Value)
	}
	// The bars take the width the labels leave, so the figure is filled.
	labelW = min(labelW, max(8, width/2))
	barW := max(6, width-labelW-textW-6)
	out := make([]Line, len(r.Bars))
	for i, b := range r.Bars {
		p := stagger(t, i, len(r.Bars))
		fill := 0
		if top > 0 {
			fill = int(math.Round(p * b.Value / top * float64(barW)))
		}
		if b.Value > 0 && fill == 0 && p > 0.5 {
			fill = 1
		}
		barSt := Plain
		if b.Style == Accent {
			barSt = Accent
		}
		label := truncate(b.Label, labelW)
		out[i] = L(
			S(pad(label, labelW), b.Style),
			S("  [", Muted),
			S(strings.Repeat("=", fill), barSt),
			S(strings.Repeat("-", barW-fill), Muted),
			S("]  ", Muted),
			S(padLeft(b.Text, textW), b.Style),
		)
	}
	return out
}

// Spark is a sparkline, ▁ to █, drawn left to right. The last point is the
// accent, as the newest value.
type Spark struct {
	Values  []float64
	Caption Line
	Wide    bool // stretch each value over up to three cells to use the width
}

const ticks = "▁▂▃▄▅▆▇█"

func (s Spark) Draw(width int, t float64) []Line {
	vals := s.Values
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	top := 0.0
	for _, v := range vals {
		top = math.Max(top, v)
	}
	cell := 1
	if s.Wide && len(vals) > 0 {
		cell = max(1, min(3, width/len(vals)))
	}
	shown := int(math.Ceil(ease(t) * float64(len(vals))))
	tk := []rune(ticks)
	var body strings.Builder
	var last string
	for i, v := range vals[:shown] {
		k := 0
		if top > 0 {
			k = int(math.Round(v / top * float64(len(tk)-1)))
		}
		c := strings.Repeat(string(tk[k]), cell)
		if v == 0 {
			c = strings.Repeat(" ", cell)
		}
		if i == len(vals)-1 {
			last = c
			continue
		}
		body.WriteString(c)
	}
	line := L(S(body.String(), Plain), S(last, Accent), S(strings.Repeat(" ", (len(vals)-shown)*cell), Plain))
	out := []Line{line}
	if s.Caption != nil {
		out = append(out, Text{s.Caption}.Draw(width, t)...)
	}
	return out
}

// Meter is one value out of a whole, [=========-----]  67%.
type Meter struct {
	Value   float64 // 0 to 1
	Text    string
	Caption Line
	Style   Style
}

func (m Meter) Draw(width int, t float64) []Line {
	barW := max(6, width-Width(m.Text)-4)
	fill := int(math.Round(ease(t) * clamp(m.Value) * float64(barW)))
	st := m.Style
	if st == Plain {
		st = Accent
	}
	out := []Line{L(S("[", Muted), S(strings.Repeat("=", fill), st), S(strings.Repeat("-", barW-fill), Muted), S("]  ", Muted), S(m.Text, st))}
	if m.Caption != nil {
		out = append(out, Text{m.Caption}.Draw(width, t)...)
	}
	return out
}

// Stat is a few big numbers side by side, each with a label under it.
// Numbers count up during the reveal.
type Stat []StatItem

type StatItem struct {
	Value int
	Unit  string // printed after the number
	Label string
	Style Style
}

func (st Stat) Draw(width int, t float64) []Line {
	var top, bottom Line
	for i, it := range st {
		v := fmt.Sprintf("%s%s", comma(int(math.Round(ease(t)*float64(it.Value)))), it.Unit)
		final := comma(it.Value) + it.Unit
		w := max(Width(final), Width(it.Label)) + 3
		if i == len(st)-1 {
			w = 0
		}
		top = append(top, S(pad(v, w), it.Style))
		bottom = append(bottom, S(pad(it.Label, w), Muted))
	}
	return []Line{top, bottom}
}

// Keys is a row of key hints: "q close  d dig".
type Keys [][2]string

func (k Keys) Draw(int, float64) []Line {
	var l Line
	for i, kv := range k {
		if i > 0 {
			l = append(l, S("   ", Plain))
		}
		l = append(l, S(kv[0], Accent), S(" "+kv[1], Muted))
	}
	return []Line{l}
}

func pad(s string, w int) string {
	if n := Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func padLeft(s string, w int) string {
	if n := Width(s); n < w {
		return strings.Repeat(" ", w-n) + s
	}
	return s
}

func comma(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + comma(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Para is text wrapped to the figure's width, on word boundaries.
type Para struct {
	Text  string
	Style Style
}

func (p Para) Draw(width int, t float64) []Line {
	var out []Line
	for _, l := range Wrap(p.Text, width) {
		out = append(out, Line{{l, p.Style}})
	}
	return Text(out).Draw(width, t)
}

// Wrap breaks text into lines at most width cells wide, on spaces where it
// can and inside a word only when the word alone is too wide.
func Wrap(text string, width int) []string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		cur := ""
		for _, w := range strings.Fields(para) {
			for Width(w) > width {
				if cur != "" {
					out, cur = append(out, cur), ""
				}
				head := truncateTo(w, width)
				out, w = append(out, head), w[len(head):]
			}
			switch {
			case cur == "":
				cur = w
			case Width(cur)+1+Width(w) <= width:
				cur += " " + w
			default:
				out, cur = append(out, cur), w
			}
		}
		out = append(out, cur)
	}
	return out
}

// Histogram is vertical bars over time, with a count axis on the left and
// labels under the bars: months, and under them years. Bars grow from the
// axis during the reveal.
type Histogram struct {
	Values []float64
	Labels []string // one per bar, cut to the bar's width
	Under  []string // a second row: a label starts under its bar and may run past it
	Height int      // rows of bars; 4 when 0
	Accent int      // the bar to accent, or -1
}

func (h Histogram) Draw(width int, t float64) []Line {
	n := len(h.Values)
	if n == 0 {
		return nil
	}
	rows := h.Height
	if rows == 0 {
		rows = 4
	}
	top := 0.0
	for _, v := range h.Values {
		top = math.Max(top, v)
	}
	topLabel := comma(int(top))
	axisW := Width(topLabel) + 1
	// Columns share the whole width; the cells that don't divide evenly go
	// one each to the first columns.
	total := max(n, width-axisW-1)
	cws := make([]int, n)
	starts := make([]int, n)
	for i, at := 0, 0; i < n; i++ {
		cws[i] = total / n
		if i < total%n {
			cws[i]++
		}
		starts[i], at = at, at+cws[i]
	}
	eighths := []rune(" ▁▂▃▄▅▆▇█")

	var out []Line
	for r := 0; r < rows; r++ {
		label, edge := strings.Repeat(" ", axisW), "│"
		if r == 0 {
			label, edge = pad(topLabel, axisW), "┤"
		}
		l := Line{{label + edge, Muted}}
		fromBottom := rows - 1 - r
		for i, v := range h.Values {
			level := 0.0
			if top > 0 {
				level = v / top * float64(rows*8) * stagger(t, i, n)
			}
			k := int(math.Round(level)) - fromBottom*8
			k = max(0, min(8, k))
			if v > 0 && fromBottom == 0 && k == 0 && t > 0.5 {
				k = 1 // a month with any commits shows
			}
			st := Plain
			if i == h.Accent {
				st = Accent
			}
			barW := max(1, cws[i]-1)
			cell := strings.Repeat(string(eighths[k]), barW)
			if cws[i] > barW {
				cell += " "
			}
			l = append(l, Span{cell, st})
		}
		out = append(out, l)
	}
	out = append(out, Line{{strings.Repeat(" ", axisW) + "└" + strings.Repeat("─", total), Muted}})
	if h.Labels != nil {
		var b strings.Builder
		for i := 0; i < n; i++ {
			s := ""
			if i < len(h.Labels) {
				s = truncateTo(h.Labels[i], cws[i])
			}
			b.WriteString(pad(s, cws[i]))
		}
		out = append(out, Line{{strings.Repeat(" ", axisW+1) + b.String(), Muted}})
	}
	if h.Under != nil {
		row := []rune(strings.Repeat(" ", total+8))
		free := 0
		for i := 0; i < n && i < len(h.Under); i++ {
			at := starts[i]
			if h.Under[i] == "" || at < free {
				continue
			}
			for j, r := range []rune(h.Under[i]) {
				if at+j < len(row) {
					row[at+j] = r
				}
			}
			free = at + len([]rune(h.Under[i])) + 1
		}
		out = append(out, Line{{strings.Repeat(" ", axisW+1) + strings.TrimRight(string(row), " "), Muted}})
	}
	return out
}
