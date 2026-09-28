package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestTargetedTestsForGoScopesByPackage(t *testing.T) {
	root := tree(t, map[string]string{"go.mod": "module x\n\ngo 1.25\n"})
	cmds, why := TargetedTests(root, []string{"pkg/a/one.go", "pkg/a/two.go", "pkg/b/x.go", "README.md"}, "")
	if len(cmds) != 1 {
		t.Fatalf("cmds = %v", cmds)
	}
	if !strings.Contains(cmds[0], "./pkg/a") || !strings.Contains(cmds[0], "./pkg/b") {
		t.Errorf("cmd = %q, want both packages", cmds[0])
	}
	if strings.Contains(cmds[0], "./.") {
		t.Errorf("cmd = %q; README.md is not a package", cmds[0])
	}
	if !strings.Contains(why, "targeted packages") {
		t.Errorf("why = %q", why)
	}
}

func TestGoWithNoSourceChangesFallsBackToTheDefault(t *testing.T) {
	root := tree(t, map[string]string{"go.mod": "module x\n\ngo 1.25\n"})
	cmds, why := TargetedTests(root, []string{"README.md"}, "")
	if len(cmds) == 0 || !strings.Contains(why, "project default") {
		t.Fatalf("cmds=%v why=%q", cmds, why)
	}
}

func TestMirroredTestsFindTheCorrespondingFile(t *testing.T) {
	root := tree(t, map[string]string{
		"pyproject.toml":               "[project]\nname='k'\n",
		"kornia/core/utils.py":         "def check_shape(): pass\n",
		"tests/core/test_utils.py":     "def test_check_shape(): pass\n",
		"tests/core/test_unrelated.py": "def test_other(): pass\n",
		"tests/__pycache__/test_x.pyc": "binary",
	})
	got := MirroredTests(root, []string{"kornia/core/utils.py"})
	var found bool
	for _, g := range got {
		if g == "tests/core/test_utils.py" {
			found = true
		}
		if strings.Contains(g, "__pycache__") || strings.HasSuffix(g, ".pyc") {
			t.Errorf("a compiled artefact was returned as a test: %s", g)
		}
	}
	if !found {
		t.Fatalf("= %v, want tests/core/test_utils.py", got)
	}
}

// TestDependentTestsFollowCallers is the kornia#4455 lesson: the changed file's
// own test passed while the real breakage was in a module that merely calls the
// edited function, and CI found it across twelve jobs.
func TestDependentTestsFollowCallers(t *testing.T) {
	root := tree(t, map[string]string{
		"pyproject.toml":            "[project]\nname='k'\n",
		"kornia/core/utils.py":      "def check_is_tensor(x): pass\n",
		"kornia/enhance/zca.py":     "from kornia.core.utils import check_is_tensor\n\nclass ZCAWhitening:\n    def fit(self, x):\n        check_is_tensor(x)\n",
		"tests/core/test_utils.py":  "def test_check(): pass\n",
		"tests/enhance/test_zca.py": "def test_zca(): pass\n",
	})
	diff := "@@ -1,3 +1,4 @@ def check_is_tensor(x):\n+    raise TypeError\n"

	got := DependentTests(root, []string{"kornia/core/utils.py"}, diff, 6)
	var found bool
	for _, g := range got {
		if g == "tests/enhance/test_zca.py" {
			found = true
		}
	}
	if !found {
		t.Fatalf("= %v, want the caller's test tests/enhance/test_zca.py", got)
	}
}

func TestDependentTestsIgnoreTheChangedFileAndTestTrees(t *testing.T) {
	root := tree(t, map[string]string{
		"pyproject.toml":       "[project]\nname='k'\n",
		"kornia/core/utils.py": "def check_is_tensor(x): pass\n",
		// A test file mentioning the symbol is an answer, not a caller to
		// chase; and the changed file mentions its own symbol by definition.
		"tests/core/test_utils.py": "from kornia.core.utils import check_is_tensor\n",
	})
	diff := "@@ @@ def check_is_tensor(x):\n+    pass\n"
	got := DependentTests(root, []string{"kornia/core/utils.py"}, diff, 6)
	if len(got) != 0 {
		t.Fatalf("= %v, want nothing: the only match was the changed file's own test", got)
	}
}

func TestNoSymbolsMeansNoDependentTests(t *testing.T) {
	root := tree(t, map[string]string{"pyproject.toml": "[project]\nname='k'\n"})
	if got := DependentTests(root, []string{"a.py"}, "@@ @@\n+x = 1\n", 6); len(got) != 0 {
		t.Fatalf("= %v", got)
	}
}

func TestTargetsAreCappedAndTheTrimIsReported(t *testing.T) {
	files := map[string]string{"pyproject.toml": "[project]\nname='k'\n"}
	var changed []string
	for i := 0; i < MaxTestTargets+7; i++ {
		p := filepath.Join("tests", "test_f"+string(rune('a'+i%26))+string(rune('0'+i/26))+".py")
		files[p] = "def test_x(): pass\n"
		changed = append(changed, filepath.ToSlash(p))
	}
	root := tree(t, files)
	cmds, why := TargetedTests(root, changed, "")
	if len(cmds) != 1 {
		t.Fatalf("cmds = %v", cmds)
	}
	if n := strings.Count(cmds[0], "tests/test_f"); n != MaxTestTargets {
		t.Errorf("ran %d files, want the cap of %d", n, MaxTestTargets)
	}
	if !strings.Contains(why, "more trimmed") {
		t.Errorf("why = %q; a silent trim hides that coverage was reduced", why)
	}
}

func TestChangedTestFilesAreRunDirectly(t *testing.T) {
	root := tree(t, map[string]string{
		"pyproject.toml":      "[project]\nname='k'\n",
		"tests/test_thing.py": "def test_thing(): pass\n",
	})
	cmds, _ := TargetedTests(root, []string{"tests/test_thing.py"}, "")
	if len(cmds) != 1 || !strings.Contains(cmds[0], "tests/test_thing.py") {
		t.Fatalf("cmds = %v", cmds)
	}
}

func TestNoToolchainIsStatedNotGuessed(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "hi"})
	cmds, why := TargetedTests(root, []string{"README.md"}, "")
	if len(cmds) != 0 || !strings.Contains(why, "no toolchain") {
		t.Fatalf("cmds=%v why=%q", cmds, why)
	}
}
