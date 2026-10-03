// Package recipe works out how to build a container that can compile and test
// a target repository.
//
// The rule that shapes everything here: a recipe must name the evidence it
// came from. Guessing an environment produces a container that fails in ways
// indistinguishable from a broken patch, and that failure costs a maintainer's
// review time rather than ours. When the evidence is absent the honest answer
// is ErrNoRecipe, and the candidate is blocked.
//
// pytorch/vision is the worked example of that refusal. Every one of its build
// and test jobs delegates to pytorch/test-infra, a repository we do not have,
// so its workflows contain no steps to read. The three workflows that do have
// steps are automation -- a PR labeller with its triggers commented out, a
// branch-pointer updater, and a nightly dataset-download probe whose
// pull_request trigger is path-filtered to three files that an ordinary source
// change never touches. None of them describes how torchvision is built.
package recipe

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	// ErrNoRecipe means no tier produced evidence. Callers must block the
	// candidate; they must not fall back to running anything on the host.
	ErrNoRecipe = errors.New("no container recipe")
	// ErrNotContainerisable means the repo's own CI only ever runs on runners
	// a Linux container cannot provide -- macOS or Windows. machine.Decide
	// turns this into a lane decision; recipe just reports it.
	ErrNotContainerisable = errors.New("recipe needs a non-Linux runner")
	// ErrIncomplete accompanies a recipe that has an image but no way to
	// verify anything with it. The recipe is still worth showing; it is not
	// worth running.
	ErrIncomplete = errors.New("recipe has no test command")
)

// Source records which tier produced a recipe. It is ordered by how much the
// evidence is worth, and it is carried into the PR audit trail.
type Source string

const (
	SourceOverride     Source = "override"
	SourceDevcontainer Source = "devcontainer"
	SourceCI           Source = "ci"
	SourceDockerfile   Source = "dockerfile"
	SourceLanguage     Source = "language"
)

// Step is one command we derived, with the reason we believe it.
type Step struct {
	// Kind is system, toolchain, install, test or lint.
	Kind string
	Run  string
	// From names the workflow file and job it came from.
	From string
}

// Tool is a language runtime the repository's CI installs for itself.
type Tool struct {
	Lang string
	// Version may be empty (CI used a version file) or a series like "3.10".
	Version string
	// VersionFile is go.mod, .python-version, .nvmrc and friends. When CI
	// reads the version from the repo we do the same rather than pinning a
	// number that drifts from the clone.
	VersionFile string
}

// Recipe is everything needed to build and drive one repository's container.
type Recipe struct {
	Repo   string
	Source Source
	// Evidence lists the files and jobs this was read out of, most specific
	// first. An empty Evidence with a non-empty recipe is a bug.
	Evidence []string

	// BaseImage is either a ref the repository itself named (devcontainer
	// image, CI `container:`) or one we chose from the toolchain.
	BaseImage string
	Platform  string

	Tools []Tool
	// System are apt packages CI installs before anything else.
	System []string

	// Setup is baked into the image. Install runs once per clone with the
	// network on. Test runs with the network off -- that run is the oracle.
	Setup   []Step
	Install []Step
	Test    []Step
	Lint    []Step

	// MaskVolumes are clone-relative paths that get a named volume mounted
	// over them, so build output never lands in the bind-mounted checkout.
	MaskVolumes []string

	// Unresolved records steps we read but refused to translate. It is
	// reported rather than executed; a long Unresolved list is the signal
	// that the recipe is thin, and it belongs in the digest.
	Unresolved []string
}

// Override is a hand-written entry from config/environments.yaml. It wins over
// every derived tier, because a human who has actually built the project knows
// more than its workflow files do.
type Override struct {
	BaseImage   string   `yaml:"base_image"`
	Platform    string   `yaml:"platform"`
	System      []string `yaml:"system"`
	Install     []string `yaml:"install"`
	Test        []string `yaml:"test"`
	Lint        []string `yaml:"lint"`
	MaskVolumes []string `yaml:"mask_volumes"`
}

// Commands is the slice of toolchain detection that recipe needs: given a
// clone, the commands that exercise it.
//
// It is declared here, at the consumer, and satisfied by a one-line adapter in
// the wiring layer. recipe therefore never imports toolchain, and toolchain
// never learns that containers exist. The previous version of this pipeline
// put every such seam in one central ports module, which nothing imported and
// which drifted out of agreement with the code it described.
type Commands interface {
	Commands(root string) (test, lint, install, masks []string)
}

// CommandsFunc adapts a plain function to Commands.
type CommandsFunc func(root string) (test, lint, install, masks []string)

func (f CommandsFunc) Commands(root string) (test, lint, install, masks []string) {
	return f(root)
}

var _ Commands = CommandsFunc(nil)

// Resolve derives a recipe for the clone at root, in tier order: a
// hand-written override, the repository's devcontainer, its own pull-request
// gates, its Dockerfile.
//
// lang is the repository's primary language as GitHub reports it; it decides
// which file extensions count as "source" when judging whether a workflow
// gates ordinary pull requests. tc may be nil.
//
// A recipe with a base image but no test command is returned together with
// ErrIncomplete: the caller must not run it, but it is worth showing, because
// it names exactly what is missing and is the input to writing an override.
func Resolve(root, repo, lang string, ov *Override, tc Commands) (Recipe, error) {
	if ov != nil {
		r := fromOverride(repo, *ov)
		fill(&r, tc, root)
		if r.Complete() {
			return r, nil
		}
		return r, fmt.Errorf("%s: override: %w", repo, ErrIncomplete)
	}

	dc, haveDC, err := fromDevcontainer(root, repo)
	if err != nil {
		return Recipe{}, err
	}
	ci, ciErr := fromCI(root, repo, lang)
	if ciErr != nil && errors.Is(ciErr, ErrNotContainerisable) && !haveDC {
		return Recipe{}, ciErr
	}

	if haveDC {
		// The two tiers answer different questions and the best recipe uses
		// both: the devcontainer says which image the maintainers develop in,
		// the workflow says which commands they gate merges on. Keeping the
		// devcontainer image and adopting CI's commands is strictly better
		// than either alone -- cli/cli is the case, where the devcontainer
		// names a Go image and go.yml names `go test -race -tags=integration`.
		if ciErr == nil {
			adopt(&dc, ci)
		}
		fill(&dc, tc, root)
		if dc.Complete() {
			return dc, nil
		}
	}
	if ciErr == nil {
		fill(&ci, tc, root)
		if ci.Complete() {
			return ci, nil
		}
	}
	if df, ok := fromDockerfile(root, repo); ok {
		fill(&df, tc, root)
		if df.Complete() {
			return df, nil
		}
	}
	if lr, ok := fromLanguage(root, repo, lang); ok {
		fill(&lr, tc, root)
		if lr.Complete() {
			return lr, nil
		}
	}
	for _, r := range []Recipe{dc, ci} {
		if r.BaseImage != "" {
			return r, fmt.Errorf("%s via %s: %w", repo, r.Source, ErrIncomplete)
		}
	}
	return Recipe{}, fmt.Errorf("%s: %w", repo, ErrNoRecipe)
}

// fromLanguage is the last tier: a recipe built from the language alone,
// when every stronger tier has failed to produce a complete one.
//
// Go only, and the asymmetry is the point rather than an omission.
//
// Go states its own toolchain in the repository, so the base image is read
// rather than guessed; `go mod download` is the whole install step for every
// Go project there is; and `go test ./...` needs no per-project knowledge.
// Nothing here is an assumption about the project -- it is the language's own
// contract. kubernetes-sigs/kind is the case: all four of its workflows build
// a real cluster inside the runner, so no CI tier can ever containerise, while
// `go test ./...` runs its unit tests perfectly well.
//
// For Python, Node and Rust there is no equivalent. The install step is where
// the project-specific knowledge lives -- which extras, which system
// libraries, which wheel index -- and a default that guesses it produces a
// patch that looks verified against an environment the maintainers do not
// have. pytorch/vision is the standing example and must keep resolving to "no
// recipe": a Python default would hand it `pip install -e .` and `pytest`, and
// the result would say nothing about the CUDA path the issue is actually in.
// A refusal the user can read is worth more than a green tick that is wrong.
func fromLanguage(root, repo, lang string) (Recipe, bool) {
	if !strings.EqualFold(lang, "go") {
		return Recipe{}, false
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return Recipe{}, false
	}
	tools := resolveVersionFiles(root, []Tool{{Lang: "go", VersionFile: ""}})
	img := toolchainImage(tools, "go")
	if img == "" {
		return Recipe{}, false
	}
	from := "language default (go.mod present)"
	return Recipe{
		Repo: repo, Source: SourceLanguage, BaseImage: img,
		Install: []Step{{Kind: "install", Run: "go mod download", From: from}},
		Test:    []Step{{Kind: "test", Run: "go test ./...", From: from}},
		Lint:    []Step{{Kind: "lint", Run: "go vet ./...", From: from}},
		Evidence: []string{
			"no stronger tier resolved; go.mod names the toolchain and " +
				"`go test ./...` is the language's own contract",
		},
	}, true
}

// adopt merges a CI-derived recipe's commands into one that already has a base
// image from a stronger tier.
func adopt(dst *Recipe, src Recipe) {
	if len(src.Test) == 0 {
		return
	}
	dst.Evidence = dedupe(append(dst.Evidence, src.Evidence...))
	dst.Tools = append(dst.Tools, src.Tools...)
	dst.System = dedupe(append(dst.System, src.System...))
	dst.Setup = append(dst.Setup, src.Setup...)
	dst.Install = append(dst.Install, src.Install...)
	dst.Test = append(dst.Test, src.Test...)
	dst.Lint = append(dst.Lint, src.Lint...)
	dst.MaskVolumes = dedupe(append(dst.MaskVolumes, src.MaskVolumes...))
	dst.Unresolved = append(dst.Unresolved, src.Unresolved...)
}

// fill supplies commands from toolchain detection for the tiers that give an
// image but no commands.
//
// The credibility check is the point. Detection would happily emit `pytest`
// for any repository with a pyproject.toml, and for kornia -- whose CI drives
// everything through `pixi run` inside a generated environment -- that would
// turn a recipe we correctly refused into one that looks complete and fails at
// collection time. So detection may only fill in where the repository has
// already told us how it installs itself, or where a human wrote the override.
func fill(r *Recipe, tc Commands, root string) {
	if tc == nil {
		return
	}
	credible := r.Source == SourceOverride || r.Source == SourceDevcontainer ||
		len(r.Install) > 0 || len(r.Test) > 0
	if !credible {
		return
	}
	test, lint, install, masks := tc.Commands(root)
	src := string(r.Source) + "+toolchain"
	if len(r.Install) == 0 {
		for _, c := range install {
			r.Install = append(r.Install, Step{Kind: "install", Run: c, From: src})
		}
	}
	if len(r.Test) == 0 {
		for _, c := range test {
			r.Test = append(r.Test, Step{Kind: "test", Run: c, From: src})
		}
	}
	if len(r.Lint) == 0 {
		for _, c := range lint {
			r.Lint = append(r.Lint, Step{Kind: "lint", Run: c, From: src})
		}
	}
	r.MaskVolumes = dedupe(append(r.MaskVolumes, masks...))
}

// LoadOverrides reads config/environments.yaml, keyed by owner/name.
func LoadOverrides(path string) (map[string]Override, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Override{}, nil
		}
		return nil, err
	}
	var doc struct {
		Repos map[string]Override `yaml:"repos"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Repos == nil {
		doc.Repos = map[string]Override{}
	}
	return doc.Repos, nil
}

func fromOverride(repo string, ov Override) Recipe {
	r := Recipe{
		Repo: repo, Source: SourceOverride,
		Evidence:    []string{"config/environments.yaml"},
		BaseImage:   ov.BaseImage,
		Platform:    ov.Platform,
		System:      ov.System,
		MaskVolumes: ov.MaskVolumes,
	}
	for _, c := range ov.Install {
		r.Install = append(r.Install, Step{Kind: "install", Run: c, From: "override"})
	}
	for _, c := range ov.Test {
		r.Test = append(r.Test, Step{Kind: "test", Run: c, From: "override"})
	}
	for _, c := range ov.Lint {
		r.Lint = append(r.Lint, Step{Kind: "lint", Run: c, From: "override"})
	}
	return r
}

// fromDockerfile is the weakest tier and deliberately narrow. A repository's
// Dockerfile usually builds a release artifact rather than a test environment,
// so it contributes a base image and nothing else -- no test command is
// invented from it.
func fromDockerfile(root, repo string) (Recipe, bool) {
	for _, name := range []string{"Dockerfile", "Dockerfile.dev", "docker/Dockerfile"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		base := firstFrom(string(b))
		if base == "" {
			continue
		}
		return Recipe{
			Repo: repo, Source: SourceDockerfile,
			Evidence:  []string{name},
			BaseImage: base,
			Unresolved: []string{
				"no test command: a repository Dockerfile builds a release " +
					"artifact, not a test environment",
			},
		}, true
	}
	return Recipe{}, false
}

func firstFrom(dockerfile string) string {
	for _, line := range strings.Split(dockerfile, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 2 && strings.EqualFold(f[0], "FROM") {
			if strings.HasPrefix(f[1], "$") {
				return "" // an ARG-parameterised base tells us nothing.
			}
			return f[1]
		}
	}
	return ""
}

// sourceExts maps a GitHub primary language to the extensions a source change
// would touch. Used only to judge path filters on pull_request triggers.
func sourceExts(lang string) []string {
	switch strings.ToLower(lang) {
	case "go":
		return []string{".go"}
	case "python":
		return []string{".py", ".pyi"}
	case "javascript":
		return []string{".js", ".jsx", ".mjs", ".cjs"}
	case "typescript":
		return []string{".ts", ".tsx", ".js"}
	case "rust":
		return []string{".rs"}
	case "java":
		return []string{".java"}
	case "c++", "cpp":
		return []string{".cpp", ".cc", ".h", ".hpp"}
	case "c":
		return []string{".c", ".h"}
	case "ruby":
		return []string{".rb"}
	case "c#", "csharp":
		return []string{".cs"}
	default:
		return nil
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
