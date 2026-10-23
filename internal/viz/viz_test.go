package viz

import (
	"regexp"
	"strings"
	"testing"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func sample() []Figure {
	return []Figure{
		{Title: "line 42", Blocks: []Block{
			Text{L(S("42 │ ", Muted), S("if err := s.exec(ctx, line); err != nil {", Plain))},
			Blank{},
			Timeline{
				{When: "2026-07-14", Text: "fix(shell): surface the failures hash was silently swallowing", Note: "Assaf · 3 months ago", Now: true},
				{When: "2026-06-02", Text: "feat: 日本語 subjects take two cells"},
				{When: "2026-01-09", Text: "first commit", Open: true},
			},
		}},
		{Title: "changes with", Blocks: []Block{
			Rank{Bars: []Bar{{Label: "internal/executor/executor.go", Value: 34, Text: "34%", Style: Accent}, {Label: "README.md", Value: 9, Text: "9%"}}},
			Spark{Values: []float64{0, 1, 4, 2, 8, 3, 5}, Caption: M("commits a month")},
			Histogram{Values: []float64{12, 1, 0, 3, 3, 9}, Labels: []string{"J", "F", "M", "A", "M", "J"}, Under: []string{"2026", "", "", "", "", ""}, Accent: 0},
			Meter{Value: 0.6, Text: "3 of 5"},
			Stat{{Value: 1234, Label: "commits"}, {Value: 7, Label: "authors"}},
			Keys{{"q", "close"}, {"d", "dig"}},
		}},
	}
}

// Every frame of the reveal has the same shape, and every line is exactly
// as wide as asked, so frames can be redrawn in place.
func TestFramesKeepTheirShape(t *testing.T) {
	for _, width := range []int{30, 60, 100} {
		var n int
		for i := 0; i <= 20; i++ {
			lines := Render(sample(), width, float64(i)/20, true)
			if i == 0 {
				n = len(lines)
			} else if len(lines) != n {
				t.Fatalf("width %d frame %d: %d lines, want %d", width, i, len(lines), n)
			}
			for _, l := range lines {
				if l == "" {
					continue
				}
				if w := Width(ansi.ReplaceAllString(l, "")); w != width {
					t.Fatalf("width %d frame %d: line is %d wide: %q", width, i, w, l)
				}
			}
		}
	}
}

func TestFinishedFigure(t *testing.T) {
	out := strings.Join(Render(sample()[:1], 60, 1, false), "\n")
	for _, want := range []string{"[ LINE 42 ]", "● ", "│", "○ ", "fix(shell)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape sequences without color:\n%s", out)
	}
	rank := strings.Join(Render(sample()[1:], 60, 1, false), "\n")
	if !strings.Contains(rank, "=]  34%") || strings.Contains(rank, "-]  34%") {
		t.Errorf("the largest bar is not full:\n%s", rank)
	}
}

func TestWrap(t *testing.T) {
	got := Wrap("hash has no history index yet; run history build --hunks there", 20)
	want := []string{"hash has no history", "index yet; run", "history build", "--hunks there"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Wrap = %q", got)
	}
	if got := Wrap("abcdefghij", 4); strings.Join(got, "|") != "abcd|efgh|ij" {
		t.Errorf("Wrap of a long word = %q", got)
	}
}

func TestHistogram(t *testing.T) {
	out := strings.Join(Render([]Figure{{Title: "activity", Blocks: []Block{
		Histogram{Values: []float64{8, 0, 2, 4}, Labels: []string{"N", "D", "J", "F"}, Under: []string{"2025", "", "2026", ""}, Accent: 0},
	}}}, 40, 1, false), "\n")
	for _, want := range []string{"8 ┤████████ ", "  └─────────────────────────────────", "    N        D       J       F", "    2025             2026"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
