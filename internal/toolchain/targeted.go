package toolchain

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// skipDirs are trees that hold no tests worth running and cost real time to
// walk: dependencies, build output, and version control.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "__pycache__": true,
	".pytest_cache": true, "target": true, "dist": true, "build": true,
	".venv": true, "venv": true, ".tox": true, ".mypy_cache": true,
	".ruff_cache": true, "site-packages": true, ".next": true, "coverage": true,
}

var testRootNames = []string{"tests", "test", "spec", "__tests__"}

// maxFileScan bounds how much of a file is read when looking for callers. A
// generated or vendored file can be megabytes, and a symbol reference past the
// first megabyte is not what this is for.
const maxFileScan = 1 << 20

// TargetedTests returns the commands that exercise this change, and a
// rationale a human can read in the report.
//
// Running the whole suite is the wrong default: a large one fails on problems
// unrelated to the diff, and CI proves the repository green in its own
// environment anyway. What matters locally is whether *this change* broke
// something.
func TargetedTests(root string, changed []string, diff string) ([]string, string) {
	ts := Detect(root)
	primary, ok := Primary(ts)
	if !ok {
		return nil, "no toolchain detected"
	}

	switch primary.Kind {
	case KindGo:
		// Go and Rust co-locate tests with the code they cover -- foo_test.go
		// beside foo.go -- so scope by the packages the diff touched rather
		// than looking for a tests/ tree that will not exist.
		pkgs := map[string]bool{}
		for _, c := range changed {
			if strings.HasSuffix(c, ".go") {
				pkgs[filepath.Dir(c)] = true
			}
		}
		if len(pkgs) == 0 {
			return primary.Test, "no Go sources changed; running the project default"
		}
		dirs := sortedKeys(pkgs)
		args := make([]string, len(dirs))
		for i, d := range dirs {
			args[i] = "./" + filepath.ToSlash(d)
		}
		return []string{"go test " + strings.Join(args, " ")},
			"targeted packages: " + joinFirst(dirs, 4)
	case KindCargo:
		return []string{"cargo test"}, "cargo test (crate-scoped)"
	}

	// Python and JavaScript keep tests in their own tree, so they have to be
	// found.
	var targets []string
	for _, c := range changed {
		if IsTestFile(c) {
			targets = append(targets, c)
		}
	}
	if len(targets) == 0 {
		targets = MirroredTests(root, changed)
	}
	targets = append(targets, DependentTests(root, changed, diff, 6)...)
	targets = dedupeTests(targets)

	if len(targets) == 0 {
		return primary.Test, "no test files identified; running the project default"
	}
	// Taking over an old pull request merges months of upstream work, so the
	// changed set can be enormous. Running two hundred files is neither
	// targeted nor fast, and CI covers the rest.
	trimmed := len(targets) - MaxTestTargets
	if trimmed > 0 {
		targets = targets[:MaxTestTargets]
	}

	rationale := fmt.Sprintf("targeted (%d", len(targets))
	if trimmed > 0 {
		rationale += fmt.Sprintf(", %d more trimmed", trimmed)
	}
	rationale += "): " + joinFirst(targets, 4)

	if primary.Kind == KindPython {
		return []string{"python -m pytest " + strings.Join(targets, " ") + " -q"}, rationale
	}
	return primary.Test, "project default"
}

// MirroredTests finds test files that correspond to changed sources:
// kornia/core/utils.py -> tests/core/test_utils.py, and anything under a test
// directory named after the changed file's own directory.
func MirroredTests(root string, changed []string) []string {
	stems := map[string]bool{}
	parents := map[string]bool{}
	for _, c := range changed {
		if filepath.Ext(c) == "" {
			continue
		}
		base := filepath.Base(c)
		stems[strings.TrimSuffix(base, filepath.Ext(base))] = true
		if p := filepath.Base(filepath.Dir(c)); p != "." && p != "/" {
			parents[p] = true
		}
	}
	if len(stems) == 0 {
		return nil
	}

	var found []string
	for _, name := range testRootNames {
		base := filepath.Join(root, name)
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			continue
		}
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil || !IsTestFile(rel) {
				return nil
			}
			fbase := d.Name()
			for s := range stems {
				if fbase == "test_"+s+filepath.Ext(fbase) ||
					strings.HasPrefix(fbase, "test_"+s+".") ||
					strings.HasPrefix(fbase, s+".test.") ||
					strings.HasPrefix(fbase, s+".spec.") {
					found = append(found, filepath.ToSlash(rel))
					return nil
				}
			}
			for p := range parents {
				if strings.Contains(filepath.ToSlash(filepath.Dir(rel)), "/"+p) ||
					filepath.Base(filepath.Dir(rel)) == p {
					found = append(found, filepath.ToSlash(rel))
					return nil
				}
			}
			return nil
		})
	}
	return dedupeTests(found)
}

// DependentTests finds tests for modules that USE what changed.
//
// Mapping a changed file to its same-named test is not enough. kornia#4455
// edited kornia/core/utils.py, whose own test file passed, while the real
// breakage was in ZCAWhitening -- a different module that merely calls the
// edited function. CI found it across twelve jobs. Following callers locally is
// the cheap way to catch that class of failure before anything is pushed.
func DependentTests(root string, changed []string, diff string, limit int) []string {
	symbols := ChangedSymbols(diff)
	if len(symbols) == 0 {
		return nil
	}
	if len(symbols) > 20 {
		symbols = symbols[:20]
	}
	pattern, err := regexp.Compile(`\b(` + strings.Join(escapeAll(symbols), "|") + `)\b`)
	if err != nil {
		return nil
	}
	changedSet := map[string]bool{}
	for _, c := range changed {
		changedSet[filepath.ToSlash(c)] = true
	}

	var callers []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if len(callers) >= limit {
			return filepath.SkipAll
		}
		switch filepath.Ext(path) {
		case ".py", ".ts", ".tsx", ".js", ".jsx", ".go", ".rs":
		default:
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// A file that changed is already covered, and a test file is an
		// answer rather than a question.
		if changedSet[rel] || IsTestFile(rel) || inTestTree(rel) {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if len(b) > maxFileScan {
			b = b[:maxFileScan]
		}
		if pattern.Match(b) {
			callers = append(callers, rel)
		}
		return nil
	})
	if len(callers) == 0 {
		return nil
	}
	return MirroredTests(root, callers)
}

func inTestTree(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		switch part {
		case "tests", "test", "spec", "__tests__":
			return true
		}
	}
	return false
}

func escapeAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = regexp.QuoteMeta(s)
	}
	return out
}

func dedupeTests(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] || !IsTestFile(s) {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinFirst(ss []string, n int) string {
	if len(ss) <= n {
		return strings.Join(ss, ", ")
	}
	return strings.Join(ss[:n], ", ") + " ..."
}
