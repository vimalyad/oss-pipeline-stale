package recipe

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vimalyad/osspipeline/internal/toolchain"
	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite golden Dockerfiles")

// tc is the toolchain seam, wired the way the pipeline wires it.
var tc = CommandsFunc(func(root string) (test, lint, install, masks []string) {
	for _, t := range toolchain.Detect(root) {
		test = append(test, t.Test...)
		lint = append(lint, t.Lint...)
		install = append(install, t.Install...)
	}
	return test, lint, install, nil
})

func fixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestResolveAgainstRealRepositories is the parity gate for this group.
//
// The four fixtures are not variations on one case; each lands in a different
// tier and one of them lands nowhere at all. Hand-written workflow files would
// agree with the parser by construction, so these are copied verbatim from the
// repositories the pipeline actually watches.
func TestResolveAgainstRealRepositories(t *testing.T) {
	tests := []struct {
		name      string
		dir, repo string
		lang      string
		wantErr   error
		wantSrc   Source
		wantImage string
		wantTest  string
	}{{
		name: "devcontainer image adopts the commands its CI gates on",
		dir:  "cli__cli", repo: "cli/cli", lang: "Go",
		wantSrc:   SourceDevcontainer,
		wantImage: "mcr.microsoft.com/devcontainers/go:1.26",
		wantTest:  "go test -race -tags=integration ./...",
	}, {
		name: "plain CI workflow derives fully",
		dir:  "huggingface__datasets", repo: "huggingface/datasets", lang: "Python",
		wantSrc:   SourceCI,
		wantImage: "python:3.10-bookworm",
		wantTest:  "python -m pytest -rfExX -m unit -n 2 --dist loadfile -sv ./tests/",
	}, {
		// Every command kornia runs is built from a step output computed from
		// a JSON-encoded matrix value. We can read the image but not the
		// commands, and inventing `pytest` here would produce a container
		// that fails at collection time and reads like a broken patch. This
		// is what config/environments.yaml exists for.
		name: "commands hidden behind step outputs stay unresolved",
		dir:  "kornia__kornia", repo: "kornia/kornia", lang: "Python",
		wantErr:   ErrIncomplete,
		wantSrc:   SourceCI,
		wantImage: "ubuntu:24.04",
	}, {
		name: "no recipe when every build job delegates off-repo",
		dir:  "pytorch__vision", repo: "pytorch/vision", lang: "Python",
		wantErr: ErrNoRecipe,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Resolve(fixture(t, tt.dir), tt.repo, tt.lang, nil, tc)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if r.Source != tt.wantSrc {
				t.Errorf("source = %q, want %q", r.Source, tt.wantSrc)
			}
			if r.BaseImage != tt.wantImage {
				t.Errorf("image = %q, want %q", r.BaseImage, tt.wantImage)
			}
			if tt.wantTest != "" {
				if !hasStep(r.Test, tt.wantTest) {
					t.Errorf("test commands = %v, want one equal to %q", runs(r.Test), tt.wantTest)
				}
				if !r.Complete() {
					t.Error("Complete() = false for a recipe with an image and a test command")
				}
			}
			if tt.wantErr == nil && len(r.Evidence) == 0 {
				t.Error("a recipe with no evidence is a guess")
			}
		})
	}
}

// TestPytorchVisionYieldsNothingUsable states the gate on its own, because the
// reason matters as much as the outcome. All twelve of its workflows are in
// testdata: eleven delegate every build job to pytorch/test-infra, and the
// three that do carry steps are automation -- a labeller with its triggers
// commented out, a branch-pointer updater, and a nightly dataset-download
// probe whose pull_request trigger names three literal files.
//
// tests-schedule.yml is the trap. It is a complete, derivable Python workflow
// -- setup-python 3.10, pip install --editable ., pytest -- and a resolver
// that only asked "can I read steps?" would emit a confident recipe that
// installs a torch nightly from a URL and runs a download test.
func TestPytorchVisionYieldsNothingUsable(t *testing.T) {
	root := fixture(t, "pytorch__vision")
	r, err := Resolve(root, "pytorch/vision", "Python", nil, tc)
	if !errors.Is(err, ErrNoRecipe) {
		t.Fatalf("err = %v, want ErrNoRecipe", err)
	}
	if r.BaseImage != "" || len(r.Test) > 0 {
		t.Fatalf("ErrNoRecipe must come with an empty recipe, got %+v", r)
	}

	wfs, _ := loadWorkflows(root)
	if len(wfs) != 12 {
		t.Fatalf("fixture drifted: %d workflows, want 12", len(wfs))
	}
	for _, w := range wfs {
		if w.gatesPullRequests("Python") && w.Path == ".github/workflows/tests-schedule.yml" {
			t.Error("tests-schedule.yml must not count as a PR gate: its paths filter " +
				"names three literal files, none of which an ordinary change touches")
		}
	}
}

// TestOnKeyIsAStringNotABoolean pins a difference from the Python version.
//
// Under YAML 1.1, which pyyaml implements, the bare word `on` resolves to the
// boolean true and a workflow's triggers arrive under the key `True`. yaml.v3
// uses the YAML 1.2 core schema and keeps it a string. Every trigger check
// here depends on that, and the bug returns silently if the library changes.
func TestOnKeyIsAStringNotABoolean(t *testing.T) {
	var m map[string]any
	if err := yaml.Unmarshal([]byte("name: x\non:\n  pull_request:\n"), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["on"]; !ok {
		t.Fatalf(`the "on" key did not survive parsing; keys = %v`, sortedKeys(m))
	}
	w, err := parseWorkflow("x.yml", []byte("on:\n  pull_request:\n    paths: ['**.go']\njobs: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ev, _, _ := w.triggers(); len(ev) != 1 || ev[0] != "pull_request" {
		t.Fatalf("triggers = %v", ev)
	}
}

func TestGatesPullRequests(t *testing.T) {
	tests := []struct {
		name, on, lang string
		want           bool
	}{
		{"bare pull_request", "on:\n  pull_request:\n", "Go", true},
		{"list form", "on: [push, pull_request]\n", "Go", true},
		{"scalar push only", "on: push\n", "Go", false},
		{"schedule only", "on:\n  schedule:\n    - cron: '0 9 * * *'\n", "Python", false},
		{"no triggers at all", "", "Python", false},
		{"source glob admits", "on:\n  pull_request:\n    paths: ['**.go']\n", "Go", true},
		{"directory glob admits", "on:\n  pull_request:\n    paths: ['src/**']\n", "Python", true},
		{"wrong extension excludes", "on:\n  pull_request:\n    paths: ['*.toml']\n", "Python", false},
		{"literal paths exclude", "on:\n  pull_request:\n    paths: ['test/one.py', 'x.md']\n", "Python", false},
		{"empty paths list admits", "on:\n  pull_request:\n    branches: [main]\n", "Python", true},
		{"unknown language falls back to globs", "on:\n  pull_request:\n    paths: ['src/**']\n", "Nim", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := parseWorkflow("t.yml", []byte(tt.on+"jobs: {}\n"))
			if err != nil {
				t.Fatal(err)
			}
			if got := w.gatesPullRequests(tt.lang); got != tt.want {
				t.Errorf("gatesPullRequests = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMatrixPicksTheLinuxLeg covers the defect that would have poisoned most
// of the watchlist. `runs-on: ${{ matrix.os }}` is how nearly every real test
// job is written; treating it as unresolvable disqualified all of them and let
// lint jobs win the ranking by default.
func TestMatrixPicksTheLinuxLeg(t *testing.T) {
	tests := []struct {
		name, matrix string
		wantOS       string
		wantLinux    bool
		wantOther    map[string]string
	}{{
		name:   "linux is chosen over the first entry",
		matrix: "os: [windows-latest, ubuntu-latest, macos-latest]",
		wantOS: "ubuntu-latest", wantLinux: true,
	}, {
		name:      "no linux leg means the job is not containerisable",
		matrix:    "os: [macos-latest, windows-latest]",
		wantLinux: false,
	}, {
		// Not arbitrary: the first entry is the leg maintainers treat as
		// primary, and for test: [unit, integration] the second one needs
		// network the verification run deliberately does not have.
		name:      "non-runner axes take the first value",
		matrix:    "test: [unit, integration]\n        python-version: ['3.10', '3.13']",
		wantLinux: true,
		wantOther: map[string]string{"matrix.test": "unit", "matrix.python-version": "3.10"},
	}, {
		name:      "include and exclude are not value sources",
		matrix:    "os: [ubuntu-latest]\n        include:\n          - os: windows-2022",
		wantOS:    "ubuntu-latest",
		wantLinux: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "jobs:\n  j:\n    runs-on: ${{ matrix.os }}\n    strategy:\n      matrix:\n        " +
				tt.matrix + "\n    steps:\n      - run: true\n"
			w, err := parseWorkflow("t.yml", []byte(src))
			if err != nil {
				t.Fatal(err)
			}
			ctx, _, linux := matrixContext(w.Jobs["j"])
			if linux != tt.wantLinux {
				t.Fatalf("linuxOK = %v, want %v", linux, tt.wantLinux)
			}
			if tt.wantOS != "" && ctx["matrix.os"] != tt.wantOS {
				t.Errorf("matrix.os = %q, want %q", ctx["matrix.os"], tt.wantOS)
			}
			for k, v := range tt.wantOther {
				if ctx[k] != v {
					t.Errorf("%s = %q, want %q", k, ctx[k], v)
				}
			}
		})
	}
}

// TestJobsNeedingSecretsAreNeverSelected: cli/cli has two jobs in go.yml that
// are nearly identical, and one of them sets GH_TOKEN. We have no secret to
// give it, so a recipe built from it would fail for a reason unrelated to the
// patch.
func TestJobsNeedingSecretsAreNeverSelected(t *testing.T) {
	r, err := Resolve(fixture(t, "cli__cli"), "cli/cli", "Go", nil, tc)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(append([]Step{}, r.Test...), r.Install...) {
		if strings.Contains(s.From, "integration-tests") {
			t.Errorf("step %q came from the token-bearing job (%s)", s.Run, s.From)
		}
		if secretRe.MatchString(s.Run) {
			t.Errorf("step %q references a secret", s.Run)
		}
	}
}

// TestToolchainFillNeedsACredibleInstall is the kornia guard. Detection would
// happily answer "pytest" for any repository with a pyproject.toml; allowing
// that to complete a recipe whose install steps we could not read turns a
// correct refusal into a container that fails at collection time.
func TestToolchainFillNeedsACredibleInstall(t *testing.T) {
	root := fixture(t, "kornia__kornia")
	if _, err := os.Stat(filepath.Join(root, "pyproject.toml")); err == nil {
		t.Fatal("fixture must not contain a pyproject.toml, or this proves nothing")
	}
	// Prove detection would in fact offer a test command for such a tree.
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "pyproject.toml"), []byte("[project]\nname='x'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if test, _, _, _ := tc.Commands(tmp); len(test) == 0 {
		t.Fatal("toolchain detection offers nothing for a python tree; test is vacuous")
	}

	r, err := Resolve(root, "kornia/kornia", "Python", nil, tc)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	if len(r.Test) != 0 {
		t.Errorf("filled in %v despite unreadable install steps", runs(r.Test))
	}
	if r.BaseImage == "" || len(r.Unresolved) == 0 {
		t.Error("an incomplete recipe must still report what it found and what it could not")
	}
}

func TestOverrideBeatsEveryDerivedTier(t *testing.T) {
	ov := &Override{
		BaseImage: "example/pinned:1",
		Install:   []string{"make deps"},
		Test:      []string{"make check"},
	}
	// cli/cli would otherwise resolve to its devcontainer image.
	r, err := Resolve(fixture(t, "cli__cli"), "cli/cli", "Go", ov, tc)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceOverride || r.BaseImage != "example/pinned:1" {
		t.Fatalf("got %s / %s", r.Source, r.BaseImage)
	}
	if !hasStep(r.Test, "make check") {
		t.Errorf("test = %v", runs(r.Test))
	}
}

func TestLoadOverrides(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "environments.yaml")
	if err := os.WriteFile(p, []byte(
		"repos:\n  kornia/kornia:\n    base_image: python:3.11-bookworm\n    test:\n      - pytest -q\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadOverrides(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["kornia/kornia"].BaseImage != "python:3.11-bookworm" {
		t.Fatalf("got %+v", m)
	}
	// A missing file is an absence of overrides, not a failure: the pipeline
	// must run on a machine where nobody has written one yet.
	if m, err := LoadOverrides(filepath.Join(dir, "nope.yaml")); err != nil || len(m) != 0 {
		t.Fatalf("missing file: %v, %v", m, err)
	}
}

func TestVersionFileIsReadFromTheClone(t *testing.T) {
	r, err := Resolve(fixture(t, "cli__cli"), "cli/cli", "Go", nil, tc)
	if err != nil {
		t.Fatal(err)
	}
	var got Tool
	for _, tl := range r.Tools {
		if tl.Lang == "go" {
			got = tl
		}
	}
	if got.VersionFile != "go.mod" {
		t.Fatalf("tools = %+v, want one reading go.mod", r.Tools)
	}
	want := goSeriesOf(t, filepath.Join(fixture(t, "cli__cli"), "go.mod"))
	if got.Version != want {
		t.Errorf("version = %q, want %q read from the clone's go.mod", got.Version, want)
	}
}

func TestRunnerImage(t *testing.T) {
	tests := []struct {
		label, image string
		linux, known bool
	}{
		{"ubuntu-latest", "ubuntu:24.04", true, true},
		{"ubuntu-22.04", "ubuntu:22.04", true, true},
		{"macos-15", "", false, true},
		{"windows-latest", "", false, true},
		{"mt-l-x86iavx512-48-384", "", false, false},
		{"self-hosted", "", false, false},
	}
	for _, tt := range tests {
		img, linux, known := runnerImage(tt.label)
		if img != tt.image || linux != tt.linux || known != tt.known {
			t.Errorf("%s -> (%q,%v,%v), want (%q,%v,%v)",
				tt.label, img, linux, known, tt.image, tt.linux, tt.known)
		}
	}
}

func TestStripJSONC(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"line comment", "{\n // hi\n \"a\":1\n}", `{"a":1}`},
		{"block comment", "{/* x */\"a\":1}", `{"a":1}`},
		{"trailing comma in object", "{\"a\":1,}", `{"a":1}`},
		{"trailing comma in array", "{\"a\":[1,2,]}", `{"a":[1,2]}`},
		// The one that matters: a naive strip turns an image ref or a URL
		// into a truncated string and then fails to parse somewhere else.
		{"slashes inside a string survive", `{"a":"https://x.io//y"}`, `{"a":"https://x.io//y"}`},
		{"escaped quote", `{"a":"b\"//c"}`, `{"a":"b\"//c"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(strings.Fields(string(stripJSONC([]byte(tt.in)))), "")
			if got != tt.want {
				t.Errorf("= %s, want %s", got, tt.want)
			}
		})
	}
}

func TestDockerfileGolden(t *testing.T) {
	cases := []struct{ dir, repo, lang string }{
		{"cli__cli", "cli/cli", "Go"},
		{"huggingface__datasets", "huggingface/datasets", "Python"},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			r, err := Resolve(fixture(t, c.dir), c.repo, c.lang, nil, tc)
			if err != nil {
				t.Fatal(err)
			}
			got := r.Dockerfile()
			golden := filepath.Join("testdata", "golden", c.dir+".Dockerfile")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run: go test ./internal/recipe -update)", err)
			}
			if got != string(want) {
				t.Errorf("Dockerfile differs from %s:\n--- got ---\n%s", golden, got)
			}
		})
	}
}

// TestDockerfileIsDeterministic: Go randomises map iteration, so anything
// rendered from a map has to be sorted or it varies run to run. A Dockerfile
// that varies means the image tag varies, which means every run rebuilds.
func TestDockerfileIsDeterministic(t *testing.T) {
	r, err := Resolve(fixture(t, "huggingface__datasets"), "huggingface/datasets", "Python", nil, tc)
	if err != nil {
		t.Fatal(err)
	}
	first, tag := r.Dockerfile(), r.ImageTag()
	for i := 0; i < 50; i++ {
		if got := r.Dockerfile(); got != first {
			t.Fatalf("Dockerfile varied on iteration %d", i)
		}
		if got := r.ImageTag(); got != tag {
			t.Fatalf("ImageTag varied on iteration %d: %s vs %s", i, got, tag)
		}
	}
	r2 := r
	r2.System = append(append([]string{}, r.System...), "libfoo")
	if r2.ImageTag() == tag {
		t.Error("a changed recipe must not reuse the previous image tag")
	}
}

// TestVolumesMaskBuildOutput: the 850 MB .venv found sitting in the kornia
// checkout is why build directories get a named volume mounted over them
// rather than being written into the bind-mounted clone.
func TestVolumesMaskBuildOutput(t *testing.T) {
	r, err := Resolve(fixture(t, "huggingface__datasets"), "huggingface/datasets", "Python", nil, tc)
	if err != nil {
		t.Fatal(err)
	}
	v := r.Volumes()
	if v["ossp-cache-pip"] != "/cache/pip" {
		t.Errorf("no shared pip cache: %v", v)
	}
	want := "/work/.venv"
	found := false
	for name, path := range v {
		if path == want {
			found = true
			if !strings.HasPrefix(name, "ossp-build-huggingface__datasets-") {
				t.Errorf("mask volume %q is not scoped to the repository", name)
			}
		}
	}
	if !found {
		t.Errorf("nothing mounted over %s: %v", want, v)
	}
}

func hasStep(steps []Step, want string) bool {
	for _, s := range steps {
		if strings.TrimSpace(s.Run) == want {
			return true
		}
	}
	return false
}

func runs(steps []Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Run)
	}
	return out
}

func goSeriesOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "go" {
			return goSeries(f[1])
		}
	}
	t.Fatalf("no go directive in %s", path)
	return ""
}

// TestPlatformScopedStepsAreFiltered: the direction of the comparison decides
// whether a step belongs to us. A substring search for "windows" gets the
// negated form backwards and drops a step the Linux leg actually runs.
func TestPlatformScopedStepsAreFiltered(t *testing.T) {
	tests := []struct {
		cond string
		skip bool
	}{
		{"", false},
		{"${{ matrix.os == 'ubuntu-latest' }}", false},
		{"${{ matrix.os == 'windows-latest' }}", true},
		{"${{ matrix.os == 'macos-15' }}", true},
		{"${{ matrix.os != 'windows-latest' }}", false},
		{"${{ runner.os == 'Windows' }}", true},
		{"${{ runner.os == 'Linux' }}", false},
		{"${{ startsWith(matrix.os, 'windows') }}", true},
		{"${{ contains(matrix.os, 'macos') }}", true},
		{"${{ github.event_name == 'push' }}", false},
	}
	for _, tt := range tests {
		if got := skipStep(step{If: tt.cond, Run: "echo hi"}); got != tt.skip {
			t.Errorf("skipStep(%q) = %v, want %v", tt.cond, got, tt.skip)
		}
	}
}

// TestAnUnresolvableGoVersionFallsBackToGoMod: helm's workflow reads
// GOLANG_VERSION out of .github/env and interpolates it, so there is nothing to
// evaluate -- while go.mod states the minimum three lines in. Defaulting
// instead picked 1.25 for a module requiring 1.26.
func TestAnUnresolvableGoVersionFallsBackToGoMod(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module helm.sh/helm/v4\n\ngo 1.26.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := resolveVersionFiles(dir, []Tool{{Lang: "go"}})
	if len(got) != 1 || got[0].Version != "1.26" {
		t.Fatalf("= %+v, want 1.26 read from go.mod", got)
	}
	// An explicit version from CI still wins: the project said it directly.
	stated := resolveVersionFiles(dir, []Tool{{Lang: "go", Version: "1.24"}})
	if stated[0].Version != "1.24" {
		t.Errorf("= %q, want the stated version", stated[0].Version)
	}
	// And a language with no manifest convention is left alone rather than
	// guessed at.
	if got := resolveVersionFiles(dir, []Tool{{Lang: "python"}}); got[0].Version != "" {
		t.Errorf("python version invented: %q", got[0].Version)
	}
}
