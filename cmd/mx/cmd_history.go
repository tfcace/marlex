package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tfcace/marlex/internal/index"
)

type commitJSON struct {
	Hash    string    `json:"hash"`
	Author  string    `json:"author"`
	Time    time.Time `json:"time"`
	Subject string    `json:"subject"`
	Merge   bool      `json:"merge"`
	Path    string    `json:"path,omitempty"` // the file's name at this commit (log)
}

func commitRow(c index.Commit) commitJSON {
	return commitJSON{Hash: c.Hash.String(), Author: c.Author, Time: c.Time, Subject: c.Subject, Merge: len(c.Parents) > 1}
}

func printCommitLine(c commitJSON, note string) {
	fmt.Printf("%.10s %s %-18.18s %s%s\n", c.Hash, day(c.Time), name(c.Author), c.Subject, note)
}

func cmdLog(e env, args []string) error {
	fs := newFlags("log", "[PATH] [--since DATE] [--follow] [--rev REV] [-n N] [--json]")
	since := fs.String("since", "", "only commits since this date or age (2026-01-02, 30d, 2w)")
	follow := fs.Bool("follow", false, "continue past renames (a file only)")
	rev := fs.String("rev", "HEAD", "only commits reachable from this revision, or \"all\"")
	n := fs.Int("n", 0, "show at most N commits (0 for all)")
	asJSON := fs.Bool("json", false, "print JSON")
	pos := parse(fs, args)
	if len(pos) > 1 {
		return usageErr(fs)
	}
	var from time.Time
	if *since != "" {
		var err error
		if from, err = parseSince(*since); err != nil {
			return err
		}
	}
	if len(pos) == 0 && *follow {
		return fmt.Errorf("--follow follows one file; name it")
	}
	ix, err := e.open()
	if err != nil {
		return err
	}
	defer ix.Close()
	scope, err := e.scope(ix, *rev)
	if err != nil {
		return err
	}
	var rows []commitJSON
	about := "" // what the commits touched, for the empty answer
	file := ""  // the file asked about, when one was: its renames get a note
	if len(pos) == 0 {
		// Every commit, newest first, as git log without a path.
		for _, idx := range ix.Since(from, scope) {
			rows = append(rows, commitRow(ix.Commit(idx)))
		}
	} else {
		t, err := e.resolvePath(ix, pos[0])
		if err != nil {
			return err
		}
		about = " touched " + fmt.Sprint(t)
		if !t.Dir {
			file = t.Path
		}
		limit := *n
		if !from.IsZero() {
			limit = 0 // the date cuts first, then -n
		}
		var entries []index.LogEntry
		switch {
		case t.Dir && *follow:
			return fmt.Errorf("--follow follows one file; %s is a directory", t)
		case t.Dir:
			entries = ix.LogDir(t.Path, scope, limit)
		default:
			entries = ix.Log(t.Path, *follow, scope, limit)
		}
		for _, en := range entries {
			if !from.IsZero() && ix.Time(en.Idx) < from.Unix() {
				continue
			}
			r := commitRow(ix.Commit(en.Idx))
			r.Path = en.Path
			rows = append(rows, r)
		}
	}
	if *n > 0 && len(rows) > *n {
		rows = rows[:*n]
	}
	if *asJSON {
		if rows == nil {
			rows = []commitJSON{}
		}
		return printJSON(rows)
	}
	for _, r := range rows {
		note := ""
		if file != "" && r.Path != file {
			note = "  [" + r.Path + "]"
		}
		printCommitLine(r, note)
	}
	if len(rows) == 0 {
		msg := "no commits reachable from " + *rev + about
		switch {
		case *since != "":
			msg += " since " + *since
		case about != "":
			msg += " (try --rev all)"
		}
		warnf("%s", msg)
	}
	return nil
}

func cmdShow(e env, args []string) error {
	fs := newFlags("show", "REV [--json]")
	asJSON := fs.Bool("json", false, "print JSON")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return usageErr(fs)
	}
	ix, err := e.open()
	if err != nil {
		return err
	}
	defer ix.Close()
	idx, err := e.commit(ix, pos[0])
	if err != nil {
		return err
	}
	c := ix.Commit(idx)
	type changeJSON struct {
		Kind    string `json:"kind"`
		Path    string `json:"path"`
		OldPath string `json:"old_path,omitempty"`
		Mode    string `json:"mode,omitempty"`
		Blob    string `json:"blob,omitempty"`
	}
	out := struct {
		commitJSON
		Parents  []string     `json:"parents"`
		Children []string     `json:"children"`
		Changes  []changeJSON `json:"changes"`
	}{commitJSON: commitRow(c), Parents: []string{}, Children: []string{}, Changes: []changeJSON{}}
	for _, p := range c.Parents {
		out.Parents = append(out.Parents, ix.Hash(p).String())
	}
	for _, k := range ix.Children(idx) {
		out.Children = append(out.Children, ix.Hash(k).String())
	}
	for _, ch := range ix.Changes(idx) {
		cj := changeJSON{Kind: string(ch.Kind), Path: ch.Path, OldPath: ch.OldPath}
		if ch.Mode != 0 {
			cj.Mode = fmt.Sprintf("%06o", ch.Mode)
		}
		if ch.HasBlob {
			cj.Blob = ch.Blob.String()
		}
		out.Changes = append(out.Changes, cj)
	}
	if *asJSON {
		return printJSON(out)
	}
	fmt.Printf("commit %s\n", out.Hash)
	for _, p := range out.Parents {
		fmt.Printf("parent %s\n", p)
	}
	for _, k := range out.Children {
		fmt.Printf("child  %s\n", k)
	}
	fmt.Printf("author %s\ndate   %s\n\n    %s\n\n", c.Author, c.Time.Format(time.RFC1123Z), c.Subject)
	for _, ch := range out.Changes {
		if ch.OldPath != "" {
			fmt.Printf("%s  %s -> %s\n", ch.Kind, ch.OldPath, ch.Path)
		} else {
			fmt.Printf("%s  %s\n", ch.Kind, ch.Path)
		}
	}
	return nil
}

func parseSince(s string) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if len(s) > 1 {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err == nil {
			unit := map[byte]time.Duration{'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour, 'y': 365 * 24 * time.Hour}[s[len(s)-1]]
			if unit > 0 {
				return time.Now().Add(-time.Duration(n) * unit), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("bad date %q (want YYYY-MM-DD, or an age like 30d, 2w, 12h, 1y)", s)
}

func cmdHotspots(e env, args []string) error {
	fs := newFlags("hotspots", "[PATH] [--since AGE] [-n N] [--json]")
	since := fs.String("since", "1y", "only count commits since this date or age")
	n := fs.Int("n", 20, "show at most N files")
	rev := fs.String("rev", "HEAD", "only commits reachable from this revision, or \"all\"")
	asJSON := fs.Bool("json", false, "print JSON")
	pos := parse(fs, args)
	if len(pos) > 1 {
		return usageErr(fs)
	}
	t, err := parseSince(*since)
	if err != nil {
		return err
	}
	ix, err := e.open()
	if err != nil {
		return err
	}
	defer ix.Close()
	under := index.PathMatch{Dir: true}
	if len(pos) == 1 {
		tg, err := e.resolvePath(ix, pos[0])
		if err != nil {
			return err
		}
		under = index.PathMatch{Path: tg.Path, Dir: tg.Dir}
	}
	scope, err := e.scope(ix, *rev)
	if err != nil {
		return err
	}
	stats := ix.Hotspots(under, t, scope, *n)
	if *asJSON {
		type row struct {
			Path    string    `json:"path"`
			Commits int       `json:"commits"`
			Authors int       `json:"authors"`
			Last    time.Time `json:"last_change"`
		}
		rows := make([]row, 0, len(stats))
		for _, s := range stats {
			rows = append(rows, row{s.Path, s.Commits, s.Authors, s.Last})
		}
		return printJSON(rows)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COMMITS\tAUTHORS\tLAST\tPATH")
	for _, s := range stats {
		fmt.Fprintf(w, "%d\t%d\t%s\t%s\n", s.Commits, s.Authors, day(s.Last), s.Path)
	}
	return w.Flush()
}

func cmdOwners(e env, args []string) error {
	fs := newFlags("owners", "PATH [-n N] [--json]\n\nPeople are ranked by their commits, each counting half as much for every 180 days of age.")
	n := fs.Int("n", 10, "show at most N people")
	rev := fs.String("rev", "HEAD", "only commits reachable from this revision, or \"all\"")
	asJSON := fs.Bool("json", false, "print JSON")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return usageErr(fs)
	}
	ix, err := e.open()
	if err != nil {
		return err
	}
	defer ix.Close()
	tg, err := e.resolvePath(ix, pos[0])
	if err != nil {
		return err
	}
	scope, err := e.scope(ix, *rev)
	if err != nil {
		return err
	}
	owners := ix.Owners(index.PathMatch{Path: tg.Path, Dir: tg.Dir}, scope, *n)
	if *asJSON {
		type row struct {
			Author  string    `json:"author"`
			Commits int       `json:"commits"`
			Score   float64   `json:"score"`
			Last    time.Time `json:"last_change"`
		}
		rows := make([]row, 0, len(owners))
		for _, a := range owners {
			rows = append(rows, row{a.Author, a.Commits, a.Score, a.Last})
		}
		return printJSON(rows)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AUTHOR\tCOMMITS\tLAST CHANGE")
	for _, a := range owners {
		fmt.Fprintf(w, "%s\t%d\t%s (%s)\n", a.Author, a.Commits, day(a.Last), ago(a.Last))
	}
	return w.Flush()
}

func cmdCoChange(e env, args []string) error {
	fs := newFlags("cochange", "FILE [-n N] [--json]")
	n := fs.Int("n", 20, "show at most N files")
	asJSON := fs.Bool("json", false, "print JSON")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return usageErr(fs)
	}
	ix, err := e.open()
	if err != nil {
		return err
	}
	defer ix.Close()
	t, err := e.resolvePath(ix, pos[0])
	if err != nil {
		return err
	}
	if t.Dir {
		return fmt.Errorf("cochange compares one file; %s is a directory", t)
	}
	pairs := ix.CoChanges(t.Path, *n)
	if *asJSON {
		type row struct {
			Path       string  `json:"path"`
			Count      int     `json:"count"`
			Confidence float64 `json:"confidence"`
		}
		rows := make([]row, 0, len(pairs))
		for _, c := range pairs {
			rows = append(rows, row{c.Path, c.Count, c.Confidence})
		}
		return printJSON(rows)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TOGETHER\tOF COMMITS\tPATH")
	for _, c := range pairs {
		fmt.Fprintf(w, "%d\t%.0f%%\t%s\n", c.Count, 100*c.Confidence, c.Path)
	}
	return w.Flush()
}

func cmdBlame(e env, args []string) error {
	fs := newFlags("blame", "FILE [-L START,END] [--json | --hashes]")
	lrange := fs.String("L", "", "only lines START,END")
	hashes := fs.Bool("hashes", false, "print only each line's commit hash")
	asJSON := fs.Bool("json", false, "print JSON")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return usageErr(fs)
	}
	ix, err := e.open()
	if err != nil {
		return err
	}
	defer func() { ix.Close() }() // ix is replaced when line ranges are added
	t, err := e.resolvePath(ix, pos[0])
	if err != nil {
		return err
	}
	if t.Dir {
		return fmt.Errorf("blame takes a file; %s is a directory", t)
	}
	ix = e.ensureHunks(ix)
	head, err := e.commit(ix, "HEAD")
	if err != nil {
		return err
	}
	// Blame needs only the line count, which the hunks give. The text is for
	// display: the working tree's copy when it matches HEAD's line count,
	// else HEAD's blob (present even in a depth-1 clone).
	n, err := ix.LineCount(t.Path, head)
	if err != nil {
		return err
	}
	lines, err := ix.Blame(t.Path, head, n)
	if err != nil {
		return err
	}
	text := make([]string, n)
	if !*hashes {
		r, err := e.gitRepo()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(t.Path)))
		if err != nil || len(splitLines(content)) != n {
			warnf("%s has uncommitted changes; showing HEAD's version", t.Path)
			if content, err = exec.Command("git", "-C", r.root, "cat-file", "blob", "HEAD:"+t.Path).Output(); err != nil {
				return fmt.Errorf("%s is not in HEAD", t.Path)
			}
		}
		if lt := splitLines(content); len(lt) == n {
			text = lt
		}
	}
	from, to := 1, n
	if *lrange != "" {
		if _, err := fmt.Sscanf(*lrange, "%d,%d", &from, &to); err != nil || from < 1 || to > n || from > to {
			return fmt.Errorf("-L: want START,END within 1..%d", n)
		}
	}
	type row struct {
		Line     int       `json:"line"`
		Hash     string    `json:"hash"`
		Author   string    `json:"author"`
		Time     time.Time `json:"time"`
		Subject  string    `json:"subject"`
		Path     string    `json:"path"`      // the file's name in that commit
		OrigLine uint32    `json:"orig_line"` // the line's number in that commit
		Boundary bool      `json:"boundary,omitempty"`
		Text     string    `json:"text"`
	}
	commits := map[uint32]index.Commit{}
	var rows []row
	for i := from - 1; i < to; i++ {
		b := lines[i]
		c, ok := commits[b.Commit]
		if !ok {
			c = ix.Commit(b.Commit)
			commits[b.Commit] = c
		}
		rows = append(rows, row{i + 1, c.Hash.String(), c.Author, c.Time, c.Subject, b.Path, b.Line, b.Boundary, text[i]})
	}
	if *asJSON {
		return printJSON(rows)
	}
	for _, r := range rows {
		if *hashes {
			fmt.Println(r.Hash)
			continue
		}
		mark := " "
		if r.Boundary {
			mark = "^"
		}
		renamed := ""
		if r.Path != t.Path {
			renamed = " " + r.Path
		}
		fmt.Printf("%s%.10s%s (%-16.16s %s %4d) %s\n", mark, r.Hash, renamed, name(r.Author), day(r.Time), r.Line, r.Text)
	}
	return nil
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// cmdQBench measures the long-lived-process case: open the index once, then
// time each query. It reads repo-relative paths from stdin and prints JSON.
// Used by bench/run.py.
func cmdQBench(e env, args []string) error {
	fs := newFlags("qbench", "[--runs N] < paths")
	runs := fs.Int("runs", 20, "repetitions per query; the best is kept")
	parse(fs, args)
	t0 := time.Now()
	ix, err := e.open()
	if err != nil {
		return err
	}
	defer ix.Close()
	open := time.Since(t0)
	scope, err := e.scope(ix, "HEAD")
	if err != nil {
		return err
	}
	head, err := e.commit(ix, "HEAD")
	if err != nil {
		return err
	}
	type row struct {
		Path       string  `json:"path"`
		Commits    int     `json:"commits"`
		LogUS      float64 `json:"log_us"`
		FollowUS   float64 `json:"follow_us"`
		Followed   int     `json:"followed"`
		CoChangeUS float64 `json:"cochange_us"`
		BlameUS    float64 `json:"blame_us,omitempty"`
		BlameLines int     `json:"blame_lines,omitempty"`
	}
	best := func(f func() int) (float64, int) {
		b, n := time.Duration(1<<62), 0
		for i := 0; i < *runs; i++ {
			t := time.Now()
			n = f()
			b = min(b, time.Since(t))
		}
		return float64(b.Nanoseconds()) / 1e3, n
	}
	out := struct {
		OpenUS float64 `json:"open_us"`
		Rows   []row   `json:"rows"`
	}{OpenUS: float64(open.Nanoseconds()) / 1e3}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	for _, p := range strings.Fields(string(data)) {
		r := row{Path: p}
		r.LogUS, r.Commits = best(func() int { return len(ix.Log(p, false, scope, 0)) })
		r.FollowUS, r.Followed = best(func() int { return len(ix.Log(p, true, scope, 0)) })
		r.CoChangeUS, _ = best(func() int { return len(ix.CoChanges(p, 20)) })
		if ix.HasHunks() {
			if n, err := ix.LineCount(p, head); err == nil && n > 0 {
				r.BlameUS, _ = best(func() int {
					n, _ := ix.LineCount(p, head)
					b, _ := ix.Blame(p, head, n)
					return len(b)
				})
				r.BlameLines = n
			}
		}
		out.Rows = append(out.Rows, r)
	}
	return printJSON(out)
}
