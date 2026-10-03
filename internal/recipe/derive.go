package recipe

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// maxCallDepth bounds how far a local `uses: ./.github/workflows/x.yml` chain
// is followed. kornia needs one hop; three is generous and stops a cycle in a
// repository we do not control from hanging a build.
const maxCallDepth = 3

// runnerImage maps a GitHub-hosted runner label to a base image. Only Ubuntu
// runners map: macOS and Windows runners are the reason ErrNotContainerisable
// exists, and a self-hosted label names hardware we cannot see.
func runnerImage(label string) (image string, linux, known bool) {
	l := strings.ToLower(strings.TrimSpace(label))
	switch {
	case l == "ubuntu-latest", l == "ubuntu-24.04", l == "ubuntu-24.04-arm":
		return "ubuntu:24.04", true, true
	case l == "ubuntu-22.04", l == "ubuntu-22.04-arm":
		return "ubuntu:22.04", true, true
	case l == "ubuntu-20.04":
		return "ubuntu:20.04", true, true
	case strings.HasPrefix(l, "macos"), strings.HasPrefix(l, "windows"):
		return "", false, true
	default:
		return "", false, false
	}
}

// setupActions maps a toolchain action to the language it installs and the
// `with:` keys that carry a version and a version file.
var setupActions = map[string]struct{ lang, verKey, fileKey string }{
	"actions/setup-go":                {"go", "go-version", "go-version-file"},
	"actions/setup-python":            {"python", "python-version", "python-version-file"},
	"actions/setup-node":              {"node", "node-version", "node-version-file"},
	"actions/setup-java":              {"java", "java-version", ""},
	"actions/setup-dotnet":            {"dotnet", "dotnet-version", "global-json-file"},
	"ruby/setup-ruby":                 {"ruby", "ruby-version", ""},
	"dtolnay/rust-toolchain":          {"rust", "toolchain", ""},
	"actions-rs/toolchain":            {"rust", "toolchain", ""},
	"prefix-dev/setup-pixi":           {"pixi", "pixi-version", ""},
	"astral-sh/setup-uv":              {"uv", "version", ""},
	"conda-incubator/setup-miniconda": {"conda", "python-version", ""},
}

// ignoredActions do something the container design already handles. checkout
// is a bind mount; cache is a named volume; the rest are reporting.
var ignoredActions = []string{
	"actions/checkout", "actions/cache", "actions/upload-artifact",
	"actions/download-artifact", "codecov/codecov-action", "actions/github-script",
	"actions/setup-qemu", "docker/setup-buildx-action", "actions/attest",
}

var (
	testCmdRe = regexp.MustCompile(`(^|[;&|\s])(pytest|go test|cargo test|npm test|npm run test|yarn test|pnpm test|tox|nox|make test|ctest|mvn test|gradle test|bundle exec rspec|python -m pytest)\b`)
	// `go mod tidy -diff` reports without writing; plain `go mod tidy` rewrites
	// go.mod and go.sum, which would put our own tooling's changes into
	// someone else's pull request. Only the reporting form may be run, so only
	// it is classified -- the bare form stays unclassified and is recorded
	// rather than executed.
	lintCmdRe  = regexp.MustCompile(`(^|[;&|\s])(go mod tidy -diff|ruff|golangci-lint|go vet|eslint|black|mypy|flake8|clippy|cargo fmt|pre-commit|gofmt|isort|shellcheck|taplo|toml-fmt)\b`)
	instCmdRe  = regexp.MustCompile(`(^|[;&|\s])(pip install|pip3 install|uv pip|uv sync|poetry install|go mod download|npm ci|npm install|yarn install|pnpm install|cargo fetch|pixi install|conda install|make deps|bundle install|python setup\.py|pip download)\b`)
	buildCmdRe = regexp.MustCompile(`(^|[;&|\s])(go build|go vet|cargo build|npm run build|yarn build|pnpm build|make build|cmake --build|mvn package|python -m build)\b`)
	aptCmdRe   = regexp.MustCompile(`(?m)^\s*(?:sudo\s+)?apt(?:-get)?\s+install\s+(.*)$`)
	// A step that reads a secret cannot be reproduced here and must not be
	// selected: we have no secrets, and a job that needs one is testing
	// something other than the code.
	secretRe = regexp.MustCompile(`secrets\.|github\.token|GITHUB_TOKEN`)
	// Steps gated on a non-Linux leg describe a platform this container is
	// not; hf/datasets has an Ubuntu ffmpeg step and a Windows conda step in
	// the same job. The direction of the comparison is the whole point:
	// `== 'windows-latest'` scopes a step to Windows and must be dropped,
	// while `!= 'windows-latest'` selects everything else, which includes us,
	// and must be kept. A plain substring search for "windows" gets the
	// second case backwards and silently drops a step we need.
	skipIfRe = regexp.MustCompile(`(?i)(==\s*['"]?[a-z0-9._-]*(windows|macos|darwin)` +
		`|(startsWith|contains|endsWith)\([^)]*(windows|macos|darwin)[^)]*\))`)
)

// candidate is one job we could build a recipe from, with its score.
type candidate struct {
	wf       *workflow
	jobName  string
	job      job
	inputs   map[string]string
	via      []string // evidence chain, outermost first
	score    int
	baseImg  string
	fromCont bool
	notes    []string
}

func (c candidate) where() string {
	return fmt.Sprintf("%s:%s", c.wf.Path, c.jobName)
}

// fromCI derives a recipe from the repository's own pull-request gates.
func fromCI(root, repo, lang string) (Recipe, error) {
	wfs, _ := loadWorkflows(root)
	if len(wfs) == 0 {
		return Recipe{}, fmt.Errorf("%s: no workflows: %w", repo, ErrNoRecipe)
	}
	byPath := map[string]*workflow{}
	for _, w := range wfs {
		byPath[w.Path] = w
	}

	var cands []candidate
	var sawNonLinux bool
	sort.Slice(wfs, func(i, j int) bool { return wfs[i].Path < wfs[j].Path })
	for _, w := range wfs {
		if w.isReusable() || !w.gatesPullRequests(lang) {
			continue
		}
		cs, nonLinux := collect(w, byPath, nil, []string{w.Path}, 0)
		sawNonLinux = sawNonLinux || nonLinux
		cands = append(cands, cs...)
	}

	for i := range cands {
		cands[i].score = scoreJob(&cands[i])
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].where() < cands[j].where()
	})

	var best *candidate
	for i := range cands {
		if cands[i].score > 0 {
			best = &cands[i]
			break
		}
	}
	if best == nil {
		if sawNonLinux {
			return Recipe{}, fmt.Errorf("%s: %w", repo, ErrNotContainerisable)
		}
		return Recipe{}, fmt.Errorf("%s: no usable CI job: %w", repo, ErrNoRecipe)
	}
	return build(root, repo, lang, *best, cands), nil
}

// collect walks one workflow's jobs, following local reusable workflows and
// recording remote ones as dead ends.
func collect(w *workflow, byPath map[string]*workflow, inputs map[string]string, via []string, depth int) ([]candidate, bool) {
	var out []candidate
	nonLinux := false
	for _, name := range sortedKeys(w.Jobs) {
		j := w.Jobs[name]
		if j.Uses != "" {
			// A remote reusable workflow lives in a repository we do not
			// have. There is nothing to read, and guessing what
			// pytorch/test-infra does is exactly the guess this package
			// exists to refuse.
			local, ok := strings.CutPrefix(j.Uses, "./")
			if !ok || depth >= maxCallDepth {
				continue
			}
			callee, ok := byPath[local]
			if !ok {
				continue
			}
			sub := callee.callInputDefaults()
			for k, v := range literalInputs(j.With) {
				sub[k] = v
			}
			cs, nl := collect(callee, byPath, sub, append(append([]string{}, via...), local+":"+name), depth+1)
			out = append(out, cs...)
			nonLinux = nonLinux || nl
			continue
		}
		if len(j.Steps) == 0 {
			continue
		}
		ctx := map[string]string{}
		for k, v := range inputs {
			ctx[k] = v
		}
		mx, notes, linuxOK := matrixContext(j)
		for k, v := range mx {
			ctx[k] = v
		}
		if !linuxOK {
			nonLinux = true
			continue
		}
		c := candidate{wf: w, jobName: name, job: j, inputs: ctx, via: via, notes: notes}
		img := containerImage(j.Container, ctx)
		if img != "" && !hasExpr(img) {
			c.baseImg, c.fromCont = img, true
		} else {
			labels := runsOnLabels(j.RunsOn, ctx)
			for _, l := range labels {
				if hasExpr(l) {
					continue
				}
				im, linux, known := runnerImage(l)
				if known && !linux {
					nonLinux = true
				}
				if linux {
					c.baseImg = im
					break
				}
			}
		}
		out = append(out, c)
	}
	return out, nonLinux
}

// scoreJob ranks a job by how much it looks like the gate a maintainer would
// point at. A job that needs a secret, or that runs nowhere we can reproduce,
// scores zero and is never selected.
func scoreJob(c *candidate) int {
	if c.baseImg == "" {
		return 0
	}
	if secretRe.MatchString(fmt.Sprint(c.job.Env)) || c.job.If != "" && secretRe.MatchString(c.job.If) {
		return 0
	}
	score := 0
	for _, s := range c.job.Steps {
		// Skip before the secret check, not after: a step scoped to Windows
		// is a step we never run, so its need for a token says nothing about
		// whether the Linux leg is reproducible here.
		if skipStep(s) {
			continue
		}
		if secretRe.MatchString(s.Run) || secretRe.MatchString(fmt.Sprint(s.Env)) {
			return 0
		}
		run := expand(s.Run, c.inputs)
		switch {
		case testCmdRe.MatchString(run):
			score += 4
		case instCmdRe.MatchString(run), buildCmdRe.MatchString(run):
			score += 2
		case lintCmdRe.MatchString(run):
			score++
		}
		if _, ok := setupActions[actionName(s.Uses)]; ok {
			score += 3
		}
	}
	if score == 0 {
		return 0
	}
	name := strings.ToLower(c.jobName + " " + c.wf.Name)
	for _, k := range []string{"test", "build", "unit", "ci"} {
		if strings.Contains(name, k) {
			score++
			break
		}
	}
	// A recipe read through a chain of local reusable workflows is real but
	// one step further from what we can see directly.
	return score - len(c.via) + 1
}

func skipStep(s step) bool {
	if s.If != "" && skipIfRe.MatchString(s.If) {
		return true
	}
	a := actionName(s.Uses)
	for _, ig := range ignoredActions {
		if strings.HasPrefix(a, ig) {
			return true
		}
	}
	return false
}

// actionName strips the @ref and any subpath so `actions/setup-go@v7` and
// `actions/setup-go@b7ad1da...` compare equal.
func actionName(uses string) string {
	if uses == "" {
		return ""
	}
	if i := strings.IndexByte(uses, '@'); i >= 0 {
		uses = uses[:i]
	}
	return uses
}

func build(root, repo, lang string, best candidate, all []candidate) Recipe {
	r := Recipe{Repo: repo, Source: SourceCI, Platform: "linux/arm64"}
	r.Evidence = append(r.Evidence, best.where())
	r.Evidence = append(r.Evidence, best.via...)
	r.Evidence = dedupe(r.Evidence)

	r.BaseImage = best.baseImg
	sys, tools, steps, unres := readSteps(best)
	r.System, r.Tools, r.Unresolved = sys, resolveVersionFiles(root, tools), unres
	for _, s := range steps {
		switch s.Kind {
		case "install":
			r.Install = append(r.Install, s)
		case "test":
			r.Test = append(r.Test, s)
		case "lint":
			r.Lint = append(r.Lint, s)
		case "system":
			r.Setup = append(r.Setup, s)
		}
	}
	// A repository that names its own container image has told us more than
	// its runner label ever could; otherwise pick a base from the toolchain,
	// because installing an arbitrary Python or Go version onto a bare Ubuntu
	// is the kind of setup that fails as a broken patch.
	if !best.fromCont {
		if im := toolchainImage(r.Tools, lang); im != "" {
			r.BaseImage = im
		}
	}
	r.MaskVolumes = dedupe(maskVolumes(r.Tools, lang))
	for _, n := range best.notes {
		r.Evidence = append(r.Evidence, "leg "+n)
	}

	// Lint often lives in its own job. Borrow it rather than shipping a
	// recipe that cannot run the check most likely to fail CI.
	if len(r.Lint) == 0 {
		for _, c := range all {
			if c.where() == best.where() || c.score <= 0 {
				continue
			}
			_, _, s2, _ := readSteps(c)
			for _, s := range s2 {
				if s.Kind == "lint" {
					r.Lint = append(r.Lint, s)
				}
			}
			if len(r.Lint) > 0 {
				r.Evidence = append(r.Evidence, c.where())
				break
			}
		}
	}
	return r
}

func readSteps(c candidate) (system []string, tools []Tool, steps []Step, unresolved []string) {
	where := c.where()
	for _, s := range c.job.Steps {
		if skipStep(s) {
			continue
		}
		if a := actionName(s.Uses); a != "" {
			spec, ok := setupActions[a]
			if !ok {
				unresolved = append(unresolved,
					fmt.Sprintf("%s: unhandled action %q (%s)", where, s.Uses, s.Name))
				continue
			}
			t := Tool{Lang: spec.lang}
			if spec.verKey != "" {
				t.Version = expand(str(s.With, spec.verKey), c.inputs)
			}
			if spec.fileKey != "" {
				t.VersionFile = str(s.With, spec.fileKey)
			}
			if hasExpr(t.Version) {
				unresolved = append(unresolved,
					fmt.Sprintf("%s: %s version is a matrix expression %q", where, spec.lang, t.Version))
				t.Version = ""
			}
			tools = append(tools, t)
			continue
		}
		run := strings.TrimSpace(expand(s.Run, c.inputs))
		if run == "" {
			continue
		}
		if m := aptCmdRe.FindAllStringSubmatch(run, -1); m != nil {
			for _, g := range m {
				for _, p := range strings.Fields(g[1]) {
					if strings.HasPrefix(p, "-") {
						continue
					}
					system = append(system, p)
				}
			}
			steps = append(steps, Step{Kind: "system", Run: run, From: where})
			continue
		}
		if hasExpr(run) {
			unresolved = append(unresolved,
				fmt.Sprintf("%s: unresolved expression in %q", where, first(run)))
			continue
		}
		switch {
		case testCmdRe.MatchString(run):
			steps = append(steps, Step{Kind: "test", Run: run, From: where})
		case lintCmdRe.MatchString(run):
			steps = append(steps, Step{Kind: "lint", Run: run, From: where})
		case instCmdRe.MatchString(run), buildCmdRe.MatchString(run):
			// A build command belongs with install: it runs with the network
			// on and warms the compile cache, and a repository that does not
			// build is not worth running tests against.
			steps = append(steps, Step{Kind: "install", Run: run, From: where})
		default:
			unresolved = append(unresolved,
				fmt.Sprintf("%s: unclassified step %q", where, first(run)))
		}
	}
	return dedupe(system), tools, steps, unresolved
}

func first(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " ..."
	}
	if len(s) > 90 {
		s = s[:90] + "..."
	}
	return strings.TrimSpace(s)
}

// toolchainImage prefers an official language image over bare Ubuntu. No
// Alpine: musl breaks manylinux wheels and cgo in ways that look like the
// patch is at fault.
func toolchainImage(tools []Tool, lang string) string {
	pick := func(l string) *Tool {
		for i := range tools {
			if tools[i].Lang == l {
				return &tools[i]
			}
		}
		return nil
	}
	// pixi, conda and uv bring their own environment resolution; a language
	// base image would fight them.
	for _, l := range []string{"pixi", "conda"} {
		if pick(l) != nil {
			return ""
		}
	}
	order := []string{"go", "python", "node", "rust", "ruby", "java"}
	if p := strings.ToLower(lang); p != "" {
		order = append([]string{p}, order...)
	}
	for _, l := range dedupe(order) {
		t := pick(l)
		if t == nil {
			continue
		}
		ver := t.Version
		if ver == "" {
			// A version file means CI reads the pin out of the clone; so do
			// we, at install time, on top of a current base.
			ver = defaultSeries[l]
		}
		if ver == "" {
			continue
		}
		switch l {
		case "go":
			return "golang:" + ver + "-bookworm"
		case "python":
			return "python:" + ver + "-bookworm"
		case "node":
			return "node:" + ver + "-bookworm"
		case "rust":
			if ver == "stable" {
				ver = defaultSeries["rust"]
			}
			return "rust:" + ver + "-bookworm"
		case "ruby":
			return "ruby:" + ver + "-bookworm"
		case "java":
			return "eclipse-temurin:" + ver + "-jdk"
		}
	}
	return ""
}

var defaultSeries = map[string]string{
	"go": "1.25", "python": "3.12", "node": "22", "rust": "1.83", "ruby": "3.3", "java": "21",
}

// maskVolumes names the directories that must not land in the bind-mounted
// clone. The 850 MB .venv found sitting in the kornia checkout is why.
func maskVolumes(tools []Tool, lang string) []string {
	has := func(l string) bool {
		for _, t := range tools {
			if t.Lang == l {
				return true
			}
		}
		return strings.EqualFold(lang, l)
	}
	var out []string
	if has("python") || has("conda") {
		out = append(out, ".venv", ".pytest_cache")
	}
	if has("pixi") {
		out = append(out, ".pixi", ".pytest_cache")
	}
	if has("node") {
		out = append(out, "node_modules")
	}
	if has("rust") {
		out = append(out, "target")
	}
	return out
}

// resolveVersionFiles reads the pin out of the clone when CI points at a
// version file rather than naming a number.
//
// The alternative is a default series, and for Go that default is actively
// harmful: the image sets GOTOOLCHAIN=local so no build reaches the network,
// so a base older than the clone's go.mod fails with a toolchain error that
// reads like a broken patch. The repository already states the answer.
func resolveVersionFiles(root string, tools []Tool) []Tool {
	for i := range tools {
		t := &tools[i]
		if t.Version != "" {
			continue
		}
		// A repository that states its version in a way we cannot evaluate
		// still states its minimum in its own manifest. helm's workflow reads
		// GOLANG_VERSION out of .github/env and interpolates it, which leaves
		// nothing to resolve -- while go.mod says `go 1.26.0` three lines in.
		// Falling back to a hardcoded default there picked 1.25 and would have
		// failed the build for a reason the repository had already answered.
		if t.VersionFile == "" && t.Lang == "go" {
			// .go-version first, because the two files mean different things.
			// go.mod's `go` directive is a *minimum*; .go-version is the
			// compiler the project actually builds with. kubernetes-sigs/kind
			// says so in a comment in its own go.mod -- "This is the go
			// language version, NOT the compiler version" -- and the two are
			// 1.17 and 1.26.7 there. Reading the floor picks a nine-release-old
			// toolchain for a repository whose CI uses the current one.
			t.VersionFile = "go.mod"
			if _, err := os.Stat(filepath.Join(root, ".go-version")); err == nil {
				t.VersionFile = ".go-version"
			}
		}
		if t.VersionFile == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, t.VersionFile))
		if err != nil {
			continue
		}
		body := string(b)
		switch {
		case strings.HasSuffix(t.VersionFile, ".go-version"):
			// A full version, so take the series the way go.mod's is taken:
			// golang:1.26.7-bookworm is not a published tag, golang:1.26 is.
			t.Version = goSeries(strings.TrimPrefix(strings.TrimSpace(body), "v"))
		case strings.HasSuffix(t.VersionFile, "go.mod"):
			for _, line := range strings.Split(body, "\n") {
				f := strings.Fields(line)
				if len(f) == 2 && f[0] == "go" {
					t.Version = goSeries(f[1])
					break
				}
			}
		default:
			t.Version = strings.TrimPrefix(strings.TrimSpace(body), "v")
		}
	}
	return tools
}

// goSeries turns a go.mod directive into an image tag. `go 1.26.0` is a valid
// directive but `golang:1.26.0-bookworm` is not always a published tag, while
// the two-component series always is.
func goSeries(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}
