package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/tfcace/marlex/internal/viz"
)

// cmdDoc is what a command's help says beyond its flags: one sentence on
// what it is for, and examples to copy.
type cmdDoc struct {
	summary  string
	examples []string
}

var docs = map[string]cmdDoc{
	"why": {"A card for a file or a line: the commit that last changed it, the file's history, the files that change with it, who knows it, and how busy it has been.",
		[]string{"mx why src/server.go:42", "mx why -i src/server.go:42     # stay open: down shows the line's previous version", "mx why src/server.go"}},
	"log": {"The commits that touched a file or a directory, newest first; without a path, every commit. --since keeps the ones since a date or an age.",
		[]string{"mx log -n 10 src/server.go", "mx log --follow src/server.go   # across renames", "mx log --since 2w", "mx log --since 2026-01-02 src/"}},
	"blame": {"Who last changed each line of a file, like git blame --first-parent.",
		[]string{"mx blame src/server.go", "mx blame -L 40,60 src/server.go"}},
	"show": {"A commit's message, author and the files it changed.",
		[]string{"mx show HEAD", "mx show a1b2c3d"}},
	"cochange": {"The files that usually change in the same commits as a file: what you probably need to touch too.",
		[]string{"mx cochange src/server.go", "mx cochange -n 5 src/server.go"}},
	"owners": {"Who has worked on a file or a directory, recent work counting most.",
		[]string{"mx owners src/", "mx owners src/server.go"}},
	"hotspots": {"The files that change most, in the whole repository or under a directory.",
		[]string{"mx hotspots", "mx hotspots src/ --since 26w"}},
	"order": {"Your change's test units, the ones most likely to fail first, with the reasons: the files that changed with them in the past, and the import graph.",
		[]string{"mx order -n 5", "mx order --files a.go,b.go", "go test $(mx order --list) ./...   # likely failures first, then everything"}},
	"build": {"Index new commits. Queries do this by themselves; build is for CI, or for line ranges up front.",
		[]string{"mx build", "mx build --hunks   # line ranges for blame, now rather than on first use"}},
	"pull": {"Fetch a published index, for a shallow clone or a sandbox without history.",
		[]string{"mx pull https://example.com/myrepo", "mx pull --hunks /mnt/bucket/myrepo"}},
	"ff": {"In a shallow clone with a pulled index: fetch just enough of this branch and index it on top.",
		[]string{"mx pull https://example.com/myrepo && mx ff"}},
	"publish": {"Publish the index, compressed, for others to pull.",
		[]string{"mx publish /mnt/bucket/myrepo"}},
	"stats": {"What the index holds and how big it is.", []string{"mx stats", "mx stats --tables"}},
	"rex install": {"Blame anything on screen in Rex: a key opens the history of the file:line nearest the cursor, in a block beside it.",
		[]string{"mx rex install", "mx rex install --key cmd+shift+y"}},
}

// hidden holds the flags help leaves out: plumbing for integrations.
var hidden = map[*flag.FlagSet]map[string]bool{}

func hide(fs *flag.FlagSet, names ...string) {
	if hidden[fs] == nil {
		hidden[fs] = map[string]bool{}
	}
	for _, n := range names {
		hidden[fs][n] = true
	}
}

// flagName is how help and errors write a flag: -n for one letter, --name
// otherwise.
func flagName(n string) string {
	if len(n) == 1 {
		return "-" + n
	}
	return "--" + n
}

// printHelp writes a command's help: usage, what it is for, examples, flags.
func printHelp(w io.Writer, fs *flag.FlagSet, name, argsUsage string) {
	fmt.Fprintf(w, "usage: mx %s %s\n", name, firstLine(argsUsage))
	d := docs[name]
	if d.summary != "" {
		fmt.Fprintf(w, "\n%s\n", strings.Join(viz.Wrap(d.summary, 76), "\n"))
	}
	if rest := restLines(argsUsage); rest != "" {
		fmt.Fprintf(w, "\n%s\n", rest)
	}
	if len(d.examples) > 0 {
		fmt.Fprintln(w, "\nexamples:")
		for _, ex := range d.examples {
			fmt.Fprintf(w, "  %s\n", ex)
		}
	}
	var rows [][2]string
	width := 0
	fs.VisitAll(func(f *flag.Flag) {
		if hidden[fs][f.Name] {
			return
		}
		left := flagName(f.Name)
		if v, _ := flag.UnquoteUsage(f); v != "" {
			left += " " + placeholder(f.Name, v)
		}
		desc := f.Usage
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
			desc += fmt.Sprintf(" (default %s)", f.DefValue)
		}
		rows = append(rows, [2]string{left, desc})
		width = max(width, len(left))
	})
	if len(rows) > 0 {
		fmt.Fprintln(w, "\nflags:")
		for _, r := range rows {
			fmt.Fprintf(w, "  %-*s  %s\n", width, r[0], r[1])
		}
	}
}

func placeholder(name, kind string) string {
	switch kind {
	case "int", "uint", "float", "duration":
		return "N"
	}
	return strings.ToUpper(name)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func restLines(s string) string {
	_, rest, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(rest)
}

// usageMsg is what a command says when its arguments are wrong: the usage
// line and an example, not the whole help.
func usageMsg(w io.Writer, name, argsUsage string) {
	fmt.Fprintf(w, "usage: mx %s %s\n", name, firstLine(argsUsage))
	if d := docs[name]; len(d.examples) > 0 {
		fmt.Fprintf(w, "  e.g. %s\n", d.examples[0])
	}
	fmt.Fprintf(w, "mx help %s shows the flags\n", name)
}

// flagError turns the flag package's complaint into one with a suggestion.
func flagError(fs *flag.FlagSet, name string, err error) error {
	msg := err.Error()
	if m := badValue.FindStringSubmatch(msg); m != nil {
		want := "a number"
		if f := fs.Lookup(m[2]); f != nil {
			if v, _ := flag.UnquoteUsage(f); v == "string" {
				want = "a value"
			}
		}
		return fmt.Errorf("mx %s: %s wants %s, not %q", name, flagName(m[2]), want, m[1])
	}
	bad, ok := strings.CutPrefix(msg, "flag provided but not defined: ")
	if !ok {
		return errors.New("mx " + name + ": " + msg)
	}
	bad = strings.TrimLeft(bad, "-")
	var names []string
	fs.VisitAll(func(f *flag.Flag) {
		if !hidden[fs][f.Name] {
			names = append(names, f.Name)
		}
	})
	msg = fmt.Sprintf("mx %s has no %s flag", name, flagName(bad))
	if s, ok := closest(bad, names); ok {
		msg += fmt.Sprintf(" (did you mean %s?)", flagName(s))
	}
	return errors.New(msg)
}

var badValue = regexp.MustCompile(`^invalid value "(.*)" for flag -+([^:]+):`)

// closest is the candidate nearest to word, when one is near enough to be
// a typo of it.
func closest(word string, cands []string) (string, bool) {
	best, bestD := "", 1<<30
	sort.Strings(cands)
	for _, c := range cands {
		if d := editDistance(word, c); d < bestD {
			best, bestD = c, d
		}
	}
	limit := max(1, len(word)/3)
	if strings.HasPrefix(best, word) && len(word) >= 2 {
		limit = len(best)
	}
	return best, best != "" && bestD <= limit
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// usages remembers each flag set's command name and argument usage, for
// help and errors.
var usages = map[*flag.FlagSet][2]string{}
