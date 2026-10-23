// Command mx answers questions about a Git repository's history from a
// memory-mapped index, without needing the history itself, and orders a
// change's tests by what that history says is likely to fail.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tfcace/marlex/internal/fb"
	"github.com/tfcace/marlex/internal/index"
)

const version = "0.1.0"

const usage = `mx remembers your repository's history, so you don't have to ask git.

It answers history questions in milliseconds, including in shallow clones and
agent sandboxes that don't have the history.

usage: mx [-C dir] [--index dir] [--no-refresh] <command> [args]

Ask about history:
  why FILE[:LINE]     a card: the line's last commit, the file's story, who knows it
  log [PATH]          commits that touched a path, or all commits (--since, --follow, -n, --rev)
  blame PATH          who last changed each line (needs build --hunks)
  show REV            a commit's metadata and changed files
  cochange FILE       files that usually change together with FILE
  owners PATH         who has worked on a file or directory, weighted by recency
  hotspots [PATH]     files with the most churn
  order               the test units your change is most likely to break, with reasons

Keep the index:
  build [--hunks]     index new commits (queries also do this automatically)
  publish DIR         publish the index, compressed, for others to pull
  pull SRC            fetch a published index, e.g. into a shallow clone
  ff                  in a shallow clone: fetch just enough of this branch and index it
  stats               what the index holds and how big it is

In Rex:
  rex install         blame anything on screen, from a key

Paths are relative to the current directory, as with git. Every query takes
--json. The index lives in .git/marlex. Run 'mx <command> -h' for flags.
`

type env struct {
	repo      string // the -C directory: git runs here and relative paths start here
	index     string // --index, overriding .git/marlex
	noRefresh bool
	st        *envState
}

func main() {
	args := os.Args[1:]
	e := env{repo: ".", noRefresh: os.Getenv("MX_NO_REFRESH") != "", st: &envState{}}
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch a := args[0]; {
		case a == "-C" && len(args) > 1:
			e.repo, args = args[1], args[2:]
		case (a == "--index" || a == "-index") && len(args) > 1:
			e.index, args = args[1], args[2:]
		case a == "--no-refresh" || a == "-no-refresh":
			e.noRefresh, args = true, args[1:]
		case a == "-h" || a == "--help":
			fmt.Print(usage)
			return
		case a == "-V" || a == "--version":
			fmt.Println("mx", version)
			return
		default:
			fmt.Fprintf(os.Stderr, "mx: unknown flag %s\n\n%s", a, usage)
			os.Exit(2)
		}
	}
	if len(args) == 0 {
		greet(e)
		return
	}
	cmds := map[string]func(env, []string) error{
		"why": cmdWhy, "log": cmdLog, "blame": cmdBlame, "show": cmdShow,
		"cochange": cmdCoChange, "owners": cmdOwners, "hotspots": cmdHotspots, "order": cmdOrder,
		"build": cmdBuild, "publish": cmdPublish, "pull": cmdPull, "ff": cmdFF, "stats": cmdStats,
		"rex":    cmdRex,
		"qbench": cmdQBench, // used by bench/run.py; not in the usage text
	}
	switch args[0] {
	case "help":
		if len(args) == 1 {
			fmt.Print(usage)
			return
		}
		// mx help CMD is mx CMD -h; rex, which has subcommands,
		// takes its one.
		h := []string{args[1]}
		if args[1] == "rex" {
			h = append(h, "install")
		}
		args = append(h, "-h")
	case "version":
		fmt.Println("mx", version)
		return
	}
	cmd, ok := cmds[args[0]]
	if !ok {
		var names []string
		for n := range cmds {
			if n != "qbench" {
				names = append(names, n)
			}
		}
		msg := fmt.Sprintf("mx: there is no %q command", args[0])
		if c, ok := closest(args[0], names); ok {
			msg += fmt.Sprintf("; did you mean mx %s?", c)
		}
		fmt.Fprintf(os.Stderr, "%s\nmx help lists them all\n", msg)
		os.Exit(2)
	}
	if err := cmd(e, args[1:]); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			os.Exit(int(ee))
		}
		fmt.Fprintln(os.Stderr, "mx:", err)
		os.Exit(1)
	}
}

type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "mx: "+format+"\n", a...)
}

// newFlags makes a command's flag set. -h prints the command's help (what
// it is for, examples, flags); a mistake prints a short usage and a
// suggestion.
func newFlags(name, argsUsage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	usages[fs] = [2]string{name, argsUsage}
	return fs
}

// parse handles flags interspersed with positional arguments. Everything
// after "--" is positional, untouched (the flag package would otherwise
// consume the "--" itself and misread what follows).
func parse(fs *flag.FlagSet, args []string) []string {
	var rest []string
	for i, a := range args {
		if a == "--" {
			args, rest = args[:i], args[i+1:]
			break
		}
	}
	u := usages[fs]
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				printHelp(os.Stdout, fs, u[0], u[1])
				os.Exit(0)
			}
			fmt.Fprintln(os.Stderr, flagError(fs, u[0], err))
			usageMsg(os.Stderr, u[0], u[1])
			os.Exit(2)
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, rest...)
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// usageErr says how to call a command, briefly, and exits with status 2.
func usageErr(fs *flag.FlagSet) error {
	u := usages[fs]
	usageMsg(os.Stderr, u[0], u[1])
	return exitError(2)
}

func (e env) gitRepo() (*gitRepo, error) { return findRepo(e.repo) }

func (e env) indexDir() (string, error) {
	if e.index != "" {
		return e.index, nil
	}
	r, err := e.gitRepo()
	if err != nil {
		return "", err
	}
	return filepath.Join(r.common, "marlex"), nil
}

var errNoIndex = errors.New("no index here yet: mx build makes one, in seconds for most repositories (in a shallow clone, mx pull brings a published one)")

func openIndex(dir string) (*index.Index, error) {
	ix, err := index.Open(dir)
	if errors.Is(err, fb.ErrVersion) {
		return nil, fmt.Errorf("%v; run mx build to rebuild the index", err)
	}
	if err != nil {
		return nil, err
	}
	if ix.NumCommits() == 0 {
		ix.Close()
		return nil, errNoIndex
	}
	return ix, nil
}

// open returns an index that describes the current HEAD. When HEAD moved
// since the last build, it indexes the new commits first (an incremental
// build takes tens of milliseconds). Where it cannot (a depth-1 clone, or
// --no-refresh), it says what the answers cover instead.
func (e env) open() (*index.Index, error) {
	dir, err := e.indexDir()
	if err != nil {
		return nil, err
	}
	ix, err := openIndex(dir)
	if errors.Is(err, errNoIndex) {
		ix, err = e.firstBuild(dir)
	}
	if err != nil {
		return nil, err
	}
	r, err := e.gitRepo()
	if err != nil {
		return ix, nil // an index used without its repository
	}
	head := e.headHash()
	if head == "" || indexedHead(ix) == head {
		return ix, nil
	}
	_, lookupErr := ix.LookupHash(head)
	known := lookupErr == nil
	if known && (e.noRefresh || r.shallow()) {
		return ix, nil // HEAD moved to a commit the index has: queries scope from it
	}
	if e.noRefresh {
		e.warnBehind(ix, r, head)
		return ix, nil
	}
	t0 := time.Now()
	res, err := index.Build(r.root, dir, index.BuildOptions{})
	if errors.Is(err, index.ErrShallowBoundary) {
		// A shallow clone whose new commits sit on its cut-off edge: they
		// can't be indexed without fetching more (mx ff).
		e.warnBehind(ix, r, head)
		return ix, nil
	}
	if err != nil {
		warnf("could not refresh the index (%v); answers may be stale", err)
		return ix, nil
	}
	if res.NewCommits > 0 {
		warnf("indexed %d new commit(s) in %s", res.NewCommits, time.Since(t0).Round(time.Millisecond))
	}
	fresh, err := openIndex(dir)
	if err != nil {
		warnf("could not reopen the refreshed index (%v); answers may be stale", err)
		return ix, nil
	}
	ix.Close()
	return fresh, nil
}

// warnBehind says what answers cover when HEAD is not indexed and the index
// can't catch up by itself.
func (e env) warnBehind(ix *index.Index, r *gitRepo, head string) {
	hint := "mx build includes it"
	if r.shallow() {
		hint = "mx ff indexes this branch on top of the pulled index"
	}
	if base, ok := e.base(ix); ok {
		// The usual pull-request checkout: HEAD is new, its parent is in
		// the published index.
		warnf("HEAD (%.10s) is not indexed yet; answers cover history up to its parent %.10s; %s", head, ix.Hash(base).String(), hint)
		return
	}
	warnf("HEAD (%.10s) is not in the index, which ends at %.10s; answers describe history up to there; %s", head, indexedHead(ix), hint)
}

func indexedHead(ix *index.Index) string {
	if ref, ok := ix.RefID("HEAD"); ok {
		return ix.Hash(ix.Reach.Tips[ref]).String()
	}
	return ""
}

// envState caches what one command learns about the repository, so HEAD is
// read once and git is asked about HEAD's parents at most once.
type envState struct {
	headRead    bool
	head        string
	parentsRead bool
	parents     []string
}

// headHash is the repository's HEAD as git sees it now ("" when unknown).
func (e env) headHash() string {
	if e.st == nil {
		e.st = &envState{} // a copy of env without a cache: no caching, still correct
	}
	if !e.st.headRead {
		e.st.headRead = true
		if r, err := e.gitRepo(); err == nil {
			e.st.head, _ = r.head()
		}
	}
	return e.st.head
}

// head is HEAD's index, when the index has HEAD itself.
func (e env) head(ix *index.Index) (uint32, bool) {
	h := e.headHash()
	if h == "" {
		return 0, false
	}
	idx, err := ix.LookupHash(h)
	return idx, err == nil
}

// base is the newest indexed commit that HEAD stands on: HEAD itself, or
// its first indexed parent (a pull-request checkout adds one commit on top
// of published history). Parents are read from HEAD's raw commit object,
// which a depth-1 clone has even though it lacks the parents themselves.
func (e env) base(ix *index.Index) (uint32, bool) {
	if idx, ok := e.head(ix); ok {
		return idx, true
	}
	if e.st == nil {
		e.st = &envState{}
	}
	if !e.st.parentsRead {
		e.st.parentsRead = true
		if r, err := e.gitRepo(); err == nil && e.st.head != "" {
			e.st.parents = r.parents(e.st.head)
		}
	}
	for _, p := range e.st.parents {
		if idx, err := ix.LookupHash(p); err == nil {
			return idx, true
		}
	}
	return 0, false
}

// commit resolves a revision. HEAD means the repository's actual HEAD, and
// it is an error, not a guess, when the index does not have it.
func (e env) commit(ix *index.Index, rev string) (uint32, error) {
	if rev == "HEAD" {
		if idx, ok := e.head(ix); ok {
			return idx, nil
		}
		if h := e.headHash(); h != "" {
			return 0, fmt.Errorf("HEAD (%.10s) is not in the index yet", h)
		}
	}
	return ix.ResolveCommit(e.repo, rev)
}

// scope limits queries to commits reachable from rev. For HEAD that is the
// history HEAD stands on, even when HEAD itself is not indexed yet.
func (e env) scope(ix *index.Index, rev string) (index.Scope, error) {
	if rev == "HEAD" {
		if idx, ok := e.base(ix); ok {
			rev = ix.Hash(idx).String()
		}
	}
	return ix.ResolveScope(e.repo, rev)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func short(h index.Hash) string { return h.String()[:10] }

func day(t time.Time) string { return t.Format("2006-01-02") }

// name is the author without the email address, for text output.
func name(author string) string {
	n, _, _ := strings.Cut(author, " <")
	return n
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
