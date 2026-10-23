package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/tfcace/marlex/internal/index"
)

// gitRepo locates a repository from the filesystem alone. Spawning git costs
// more than a query, so the hot path reads .git directly and falls back to
// git only for formats it does not parse (reftable).
type gitRepo struct {
	root   string // worktree top level
	gitDir string // per-worktree directory holding HEAD
	common string // shared directory holding refs and the index
}

func findRepo(start string) (*gitRepo, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		dir = d
	}
	for {
		p := filepath.Join(dir, ".git")
		st, err := os.Stat(p)
		if err == nil && st.IsDir() {
			return &gitRepo{root: dir, gitDir: p, common: p}, nil
		}
		if err == nil {
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
			if !ok {
				return nil, fmt.Errorf("%s: unrecognized .git file", p)
			}
			if !filepath.IsAbs(gitdir) {
				gitdir = filepath.Join(dir, gitdir)
			}
			r := &gitRepo{root: dir, gitDir: filepath.Clean(gitdir), common: filepath.Clean(gitdir)}
			if c, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
				common := strings.TrimSpace(string(c))
				if !filepath.IsAbs(common) {
					common = filepath.Join(gitdir, common)
				}
				r.common = filepath.Clean(common)
			}
			return r, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("%s is not inside a git repository (pass --index to use an index without one)", start)
		}
		dir = parent
	}
}

// head returns the commit HEAD points at, or "" on an unborn branch.
func (r *gitRepo) head() (string, error) {
	data, err := os.ReadFile(filepath.Join(r.gitDir, "HEAD"))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	ref, symbolic := strings.CutPrefix(s, "ref: ")
	if !symbolic {
		return s, nil // detached
	}
	return r.resolveRef(ref)
}

func (r *gitRepo) resolveRef(ref string) (string, error) {
	if _, err := os.Stat(filepath.Join(r.common, "reftable")); err == nil {
		out, err := exec.Command("git", "-C", r.root, "rev-parse", "--verify", "-q", ref).Output()
		if err != nil {
			return "", nil
		}
		return strings.TrimSpace(string(out)), nil
	}
	if data, err := os.ReadFile(filepath.Join(r.common, filepath.FromSlash(ref))); err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	f, err := os.Open(filepath.Join(r.common, "packed-refs"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil // unborn branch
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		hash, name, ok := strings.Cut(sc.Text(), " ")
		if ok && name == ref {
			return hash, nil
		}
	}
	return "", sc.Err()
}

// parents lists a commit's parent hashes from its raw object. A depth-1
// clone has HEAD's object but grafts away its parents, so rev-parse HEAD^
// fails there; the object still names them.
func (r *gitRepo) parents(hash string) []string {
	out, err := exec.Command("git", "-C", r.root, "cat-file", "-p", hash).Output()
	if err != nil {
		return nil
	}
	var ps []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			break // end of the header
		}
		if p, ok := strings.CutPrefix(line, "parent "); ok {
			ps = append(ps, p)
		}
	}
	return ps
}

func (r *gitRepo) shallow() bool {
	_, err := os.Stat(filepath.Join(r.common, "shallow"))
	return err == nil
}

// target is what a path argument names in the index.
type target struct {
	Path string // repo-relative, slash-separated; "" is the whole repository
	Dir  bool
}

func (t target) String() string {
	switch {
	case t.Path == "":
		return "the repository"
	case t.Dir:
		return t.Path + "/"
	}
	return t.Path
}

// resolvePath maps a path argument to a file or directory with history.
// Like git, a relative path is relative to the current directory (the -C
// directory if given). Agents often pass repo-relative paths from anywhere,
// so that reading is tried second. A path with no history is an error with a
// suggestion, never an empty answer.
func (e env) resolvePath(ix *index.Index, arg string) (target, error) {
	cands := e.pathCandidates(arg)
	for _, c := range cands {
		if c == "." {
			return target{Path: "", Dir: true}, nil
		}
		if ix.HasHistory(c) {
			return target{Path: c}, nil
		}
		if ix.HasPathsUnder(c) {
			return target{Path: c, Dir: true}, nil
		}
	}
	if len(cands) == 0 {
		return target{}, fmt.Errorf("%s is outside the repository", arg)
	}
	msg := fmt.Sprintf("%s has no history in the index", cands[0])
	if s, ok := ix.Suggest(cands[0]); ok {
		msg += fmt.Sprintf(" (did you mean %s?)", s)
	}
	return target{}, errors.New(msg)
}

// pathCandidates is what a path argument may name, repo-relative: relative
// to the current directory first, then to the repository's root.
func (e env) pathCandidates(arg string) []string {
	var cands []string
	if r, err := e.gitRepo(); err == nil {
		abs := arg
		if !filepath.IsAbs(abs) {
			cwd, _ := filepath.Abs(e.repo)
			if d, err := filepath.EvalSymlinks(cwd); err == nil {
				cwd = d
			}
			abs = filepath.Join(cwd, arg)
		} else if d, err := filepath.EvalSymlinks(abs); err == nil {
			abs = d
		}
		if rel, err := filepath.Rel(r.root, abs); err == nil && !strings.HasPrefix(rel, "..") {
			cands = append(cands, filepath.ToSlash(rel))
		}
	}
	if !filepath.IsAbs(arg) {
		cands = append(cands, path.Clean(filepath.ToSlash(arg)))
	}
	return cands
}
