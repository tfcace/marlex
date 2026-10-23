package rank

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Unit is one thing a test runner can be told to run: a Go package or a test
// file.
type Unit struct {
	Name      string   // what the runner receives and what reports name
	TestFiles []string // repo-relative, used for co-change affinity
	// Distance is the shortest import path from the unit's tests to a changed
	// file: 0 when the unit itself changed, -1 when unreachable.
	Distance int
	// Module is the repo-relative directory of the Go module that owns the
	// unit ("." for the root); empty for non-Go units.
	Module string
}

// Adapter is the language-specific part: discovering units and the static
// dependency graph. Everything else is shared.
type Adapter interface {
	Name() string
	Units(repo string, changed []string) ([]Unit, error)
}

func Adapters(names []string) ([]Adapter, error) {
	var out []Adapter
	for _, n := range names {
		switch n {
		case "go":
			out = append(out, GoAdapter{})
		case "files", "jest", "vitest", "pytest":
			out = append(out, FilesAdapter{})
		default:
			return nil, fmt.Errorf("unknown adapter %q (want go or files)", n)
		}
	}
	return out, nil
}

// DetectAdapters picks adapters from marker files at the repo root.
func DetectAdapters(repo string) []Adapter {
	var out []Adapter
	files, _ := gitFiles(repo)
	hasGo, hasFiles := false, false
	for _, f := range files {
		switch {
		case path.Base(f) == "go.mod":
			hasGo = true
		case isJSTest(f) || isPyTest(f):
			hasFiles = true
		}
	}
	if hasGo {
		out = append(out, GoAdapter{})
	}
	if hasFiles {
		out = append(out, FilesAdapter{})
	}
	return out
}

func gitFiles(repo string) ([]string, error) {
	out, err := exec.Command("git", "-C", repo, "-c", "core.quotePath=false", "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	return files, nil
}

// ---- Go ----

type GoAdapter struct{}

func (GoAdapter) Name() string { return "go" }

type goPackage struct {
	ImportPath   string
	Dir          string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
	TestImports  []string
	XTestImports []string
	module       string // repo-relative go.mod directory
}

func (GoAdapter) Units(repo string, changed []string) ([]Unit, error) {
	files, err := gitFiles(repo)
	if err != nil {
		return nil, err
	}
	absRepo, _ := filepath.Abs(repo)
	absRepo, _ = filepath.EvalSymlinks(absRepo)
	var pkgs []goPackage
	var firstErr error
	for _, f := range files {
		if path.Base(f) != "go.mod" || strings.Contains(f, "/testdata/") || strings.HasPrefix(f, "testdata/") {
			continue
		}
		cmd := exec.Command("go", "list", "-e", "-json", "./...")
		cmd.Dir = filepath.Join(repo, path.Dir(f))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			// A module that can't load (an example whose dependencies only
			// resolve inside the project's own build) is left out rather
			// than sinking the ranking of every other module.
			if firstErr == nil {
				firstErr = fmt.Errorf("go list in %s: %v: %s", cmd.Dir, err, stderr.String())
			}
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var p goPackage
			if err := dec.Decode(&p); err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}
			if d, err := filepath.EvalSymlinks(p.Dir); err == nil {
				p.Dir = d
			}
			rel, err := filepath.Rel(absRepo, p.Dir)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			p.Dir = filepath.ToSlash(rel)
			p.module = path.Dir(f)
			pkgs = append(pkgs, p)
		}
	}

	if len(pkgs) == 0 && firstErr != nil {
		return nil, firstErr
	}
	byDir := map[string]string{}
	for _, p := range pkgs {
		byDir[p.Dir] = p.ImportPath
	}
	// Reverse edges: dependency -> packages whose tests depend on it.
	rdeps := map[string][]string{}
	for _, p := range pkgs {
		seen := map[string]bool{p.ImportPath: true}
		for _, list := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
			for _, imp := range list {
				if !seen[imp] {
					seen[imp] = true
					rdeps[imp] = append(rdeps[imp], p.ImportPath)
				}
			}
		}
	}

	dist := map[string]int{}
	var queue []string
	moduleWide := false
	for _, f := range changed {
		if b := path.Base(f); b == "go.mod" || b == "go.sum" || b == "go.work" {
			moduleWide = true
			continue
		}
		// The nearest enclosing package owns the file (covers testdata/).
		for d := path.Dir(f); ; d = path.Dir(d) {
			if ip, ok := byDir[d]; ok {
				if _, seen := dist[ip]; !seen {
					dist[ip] = 0
					queue = append(queue, ip)
				}
				break
			}
			if d == "." || d == "/" {
				break
			}
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, r := range rdeps[cur] {
			if _, seen := dist[r]; !seen {
				dist[r] = dist[cur] + 1
				queue = append(queue, r)
			}
		}
	}

	var units []Unit
	for _, p := range pkgs {
		if len(p.TestGoFiles)+len(p.XTestGoFiles) == 0 {
			continue
		}
		u := Unit{Name: p.ImportPath, Distance: -1, Module: p.module}
		for _, f := range append(append([]string{}, p.TestGoFiles...), p.XTestGoFiles...) {
			u.TestFiles = append(u.TestFiles, path.Join(p.Dir, f))
		}
		if d, ok := dist[p.ImportPath]; ok {
			u.Distance = d
		} else if moduleWide {
			u.Distance = 2
		}
		units = append(units, u)
	}
	return units, nil
}

// ---- Jest / Vitest / pytest ----

// FilesAdapter treats each test file as a unit and builds a file-level import
// graph with a regex scan. It resolves relative JS/TS imports and Python
// module imports; tsconfig path aliases and workspace packages are not
// resolved in this POC.
type FilesAdapter struct{}

func (FilesAdapter) Name() string { return "files" }

var (
	jsTestRe = regexp.MustCompile(`\.(test|spec)\.(c|m)?(j|t)sx?$`)
	jsSrcRe  = regexp.MustCompile(`\.(c|m)?(j|t)sx?$`)
	jsImport = regexp.MustCompile(`(?:import|export)\s[^'"]*?from\s*['"]([^'"]+)['"]|import\s*\(?\s*['"]([^'"]+)['"]|require\(\s*['"]([^'"]+)['"]\s*\)`)
	pyFrom   = regexp.MustCompile(`(?m)^\s*from\s+(\.*[\w.]*)\s+import\s+([\w, ()*]+)`)
	pyImport = regexp.MustCompile(`(?m)^\s*import\s+([\w.]+(?:\s*,\s*[\w.]+)*)`)
	jsExts   = []string{"", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", "/index.ts", "/index.tsx", "/index.js"}
)

func isJSTest(f string) bool { return jsTestRe.MatchString(f) && !strings.Contains(f, "node_modules/") }

func isPyTest(f string) bool {
	b := path.Base(f)
	return strings.HasSuffix(b, ".py") && (strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py"))
}

func (FilesAdapter) Units(repo string, changed []string) ([]Unit, error) {
	files, err := gitFiles(repo)
	if err != nil {
		return nil, err
	}
	exists := map[string]bool{}
	for _, f := range files {
		exists[f] = true
	}
	pyRoots := []string{"", "src/"}

	resolvePy := func(from, mod string) []string {
		var bases []string
		if strings.HasPrefix(mod, ".") {
			dots := len(mod) - len(strings.TrimLeft(mod, "."))
			dir := path.Dir(from)
			for i := 1; i < dots; i++ {
				dir = path.Dir(dir)
			}
			rest := strings.ReplaceAll(strings.TrimLeft(mod, "."), ".", "/")
			bases = append(bases, path.Join(dir, rest))
		} else {
			for _, root := range pyRoots {
				bases = append(bases, root+strings.ReplaceAll(mod, ".", "/"))
			}
		}
		var out []string
		for _, b := range bases {
			for _, c := range []string{b + ".py", b + "/__init__.py"} {
				if exists[path.Clean(c)] {
					out = append(out, path.Clean(c))
				}
			}
		}
		return out
	}

	// deps[file] = files it imports.
	rdeps := map[string][]string{}
	for _, f := range files {
		isJS, isPy := jsSrcRe.MatchString(f) && !strings.Contains(f, "node_modules/"), strings.HasSuffix(f, ".py")
		if !isJS && !isPy {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			continue
		}
		var deps []string
		if isJS {
			for _, m := range jsImport.FindAllSubmatch(src, -1) {
				spec := string(bytes.Join(m[1:], nil))
				if !strings.HasPrefix(spec, ".") {
					continue
				}
				base := path.Join(path.Dir(f), spec)
				for _, ext := range jsExts {
					if exists[base+ext] {
						deps = append(deps, base+ext)
						break
					}
				}
			}
		} else {
			for _, m := range pyFrom.FindAllSubmatch(src, -1) {
				mod := string(m[1])
				deps = append(deps, resolvePy(f, mod)...)
				// `from pkg import mod` may name a submodule.
				for _, name := range strings.FieldsFunc(string(m[2]), func(r rune) bool { return r == ',' || r == ' ' || r == '(' || r == ')' }) {
					sub := mod + "." + name
					if strings.HasSuffix(mod, ".") {
						sub = mod + name
					}
					deps = append(deps, resolvePy(f, sub)...)
				}
			}
			for _, m := range pyImport.FindAllSubmatch(src, -1) {
				for _, mod := range strings.Split(string(m[1]), ",") {
					deps = append(deps, resolvePy(f, strings.TrimSpace(mod))...)
				}
			}
		}
		for _, d := range deps {
			if d != f {
				rdeps[d] = append(rdeps[d], f)
			}
		}
	}

	dist := map[string]int{}
	var queue []string
	global := false
	push := func(f string, d int) {
		if _, ok := dist[f]; !ok {
			dist[f] = d
			queue = append(queue, f)
		}
	}
	for _, f := range changed {
		switch path.Base(f) {
		case "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "pyproject.toml", "requirements.txt", "setup.py", "setup.cfg", "tsconfig.json":
			global = true
		case "conftest.py":
			// pytest fixtures apply to every test below this directory.
			dir := path.Dir(f)
			for _, t := range files {
				if isPyTest(t) && (dir == "." || strings.HasPrefix(t, dir+"/")) {
					push(t, 1)
				}
			}
		}
		push(f, 0)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, r := range rdeps[cur] {
			push(r, dist[cur]+1)
		}
	}

	var units []Unit
	for _, f := range files {
		if !isJSTest(f) && !isPyTest(f) {
			continue
		}
		u := Unit{Name: f, TestFiles: []string{f}, Distance: -1}
		if d, ok := dist[f]; ok {
			u.Distance = d
		} else if global {
			u.Distance = 3
		}
		units = append(units, u)
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	return units, nil
}
