// Package implement writes the patch.
//
// The brief is the spec, not a suggestion: MaintainerDesiredApproach is what to
// build, RejectedApproaches are hard prohibitions, AcceptanceCriteria is the
// definition of done. A patch that contradicts a refusal is blocked here rather
// than argued about on the pull request.
//
// The standing rule for the diff is match the host repository. SOLID is the
// quality bar for code this project writes for itself; it is never a licence to
// restructure code we merely touch. Turning up to a mature codebase with a
// preferred architecture is one of the fastest ways to get a pull request
// closed.
//
// The agent never executes anything. It runs on the host with Bash disabled and
// may only read and write files in the clone; every command is run afterwards by
// this package, inside a container. That makes "no target-repo code runs on the
// host" a property of the process rather than a rule an agent is asked to
// follow, and it means every command run against someone else's repository is a
// logged decision of ours.
package implement

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/vimalyad/osspipeline/internal/llm"
	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/repro"
	"github.com/vimalyad/osspipeline/internal/sandbox"
	"github.com/vimalyad/osspipeline/internal/text"
	"github.com/vimalyad/osspipeline/internal/toolchain"
)

//go:embed prompts/patch.md
var patchPrompt string

//go:embed prompts/verify.md
var verifyPrompt string

var (
	ErrImplement = errors.New("implement")
	// ErrNoBrief means there is no spec to build against. Refusing is correct:
	// a patch written from the issue title alone is a guess.
	ErrNoBrief = errors.New("no brief to implement against")
)

// Agent writes the patch. It has no shell.
type Agent interface {
	Patch(ctx context.Context, clone, prompt string) (string, error)
}

// Judge performs the independent check that the patch honoured the refusals.
type Judge interface {
	JudgeJSON(ctx context.Context, prompt string, v any) error
}

// Git reads the working tree.
type Git interface {
	Diff(ctx context.Context, dir string) string
	NameStatus(ctx context.Context, dir string) (committed, pending string)
}

// Runner executes commands in the sandbox. Two are expected: one session with
// the network, for installing dependencies, and one without, for verification.
// Two parameters rather than a flag, so a caller cannot verify a patch against
// a session that still has egress -- the whole value of the offline run is that
// a test which only passes with network access has not verified anything.
type Runner interface {
	Run(ctx context.Context, cmd string) (sandbox.Result, error)
}

// Verdict is the independent check's answer.
type Verdict struct {
	Violates         []string `json:"violates"`
	TouchesWorkflows bool     `json:"touches_workflows"`
	HasTest          bool     `json:"has_test"`
	AIMentions       []string `json:"ai_mentions"`
	Summary          string   `json:"summary"`
}

// Result is everything one implement run produced.
type Result struct {
	OK        bool
	Summary   string
	DiffEmpty bool
	// Refusal is the agent's stated reason for changing nothing, kept from the
	// start rather than the end: a refusal leads with its conclusion, and
	// truncating from the end once left a fragment of a sentence as the entire
	// recorded reason for declining a real candidate.
	Refusal   string
	Toolchain string
	TestScope string
	Tests     []repro.Result
	// EnvironmentFailure says the container could not build the repository,
	// which is a fact about the repository and not about the diff.
	EnvironmentFailure bool
	Verdict            Verdict
	Blocked            []string
}

// Options tune a run.
type Options struct {
	// Body is the issue body, already bounded by the caller.
	Body string
	Log  func(string)
}

func (o Options) logf(f string, a ...any) {
	if o.Log != nil {
		o.Log(fmt.Sprintf(f, a...))
	}
}

// Prompt builds the patch prompt. Exported so a dry run can show exactly what
// the agent will be asked, without asking it.
func Prompt(c *model.Candidate, commands []string, body string) (string, error) {
	spec := SpecBlock(c)
	if spec == "" {
		return "", fmt.Errorf("%w: %s", ErrNoBrief, c.Slug())
	}
	cmdBlock := "  (none detected)"
	if len(commands) > 0 {
		lines := make([]string, len(commands))
		for i, cmd := range commands {
			lines[i] = "  " + cmd
		}
		cmdBlock = strings.Join(lines, "\n")
	}
	return llm.Render(patchPrompt, map[string]string{
		"repo": c.Repo, "issue": fmt.Sprint(c.Issue), "title": c.Title,
		"url": c.URL, "body": text.Clip(body, 4000),
		"spec": spec, "commands": cmdBlock,
	}), nil
}

// SpecBlock renders the brief as the instruction set the patch is written
// against. Empty when there is no usable spec, which is a refusal rather than a
// reason to improvise.
func SpecBlock(c *model.Candidate) string {
	b := c.Brief
	if b == nil {
		return ""
	}
	var out []string
	if b.MaintainerDesiredApproach != "" {
		out = append(out, fmt.Sprintf("MAINTAINER WANTS (%s): %q",
			b.ApproachAuthorAssociation, b.MaintainerDesiredApproach))
		if b.ApproachSourceURL != "" {
			out = append(out, "  source: "+b.ApproachSourceURL)
		}
	}
	for _, r := range b.RejectedApproaches {
		out = append(out, "MUST NOT: "+r)
	}
	for _, a := range b.AcceptanceCriteria {
		out = append(out, "DONE WHEN: "+a)
	}
	if b.Reproduction != "" {
		out = append(out, "REPRODUCTION: "+b.Reproduction)
	}
	return strings.Join(out, "\n")
}

// Run writes the patch and exercises it.
//
// baseline is the test results from before the patch, so only new failures
// count. Without it, a container that disagrees with the maintainer's CI --
// a wrong wheel build, a missing optional dependency -- makes every pre-existing
// red test look like something this change broke.
func Run(ctx context.Context, a Agent, j Judge, g Git, net, offline Runner,
	c *model.Candidate, clone string, baseline []repro.Result, o Options) (Result, error) {

	ts := toolchain.Detect(clone)
	primary, _ := toolchain.Primary(ts)
	var commands []string
	commands = append(commands, primary.Test...)
	commands = append(commands, primary.Lint...)

	prompt, err := Prompt(c, commands, o.Body)
	if err != nil {
		return Result{}, err
	}

	out, err := a.Patch(ctx, clone, prompt)
	res := Result{Summary: text.Clip(strings.TrimSpace(out), 3000), Toolchain: string(primary.Kind)}
	if err != nil {
		return res, fmt.Errorf("%w: %v", ErrImplement, err)
	}

	diff := g.Diff(ctx, clone)
	if strings.TrimSpace(diff) == "" {
		res.DiffEmpty = true
		res.Refusal = text.Clip(strings.TrimSpace(out), 4000)
		return res, nil
	}

	changed := changedFiles(ctx, g, clone)
	cmds, rationale := toolchain.TargetedTests(clone, changed, diff)
	res.TestScope = rationale
	o.logf("    tests -> %s", rationale)

	// Dependencies first, with the network; then the oracle without it.
	for _, cmd := range primary.Install {
		if _, err := net.Run(ctx, cmd); err != nil {
			return res, fmt.Errorf("%w: install %q: %v", ErrImplement, cmd, err)
		}
	}
	for _, cmd := range append(append([]string{}, cmds...), primary.Lint...) {
		r, err := offline.Run(ctx, cmd)
		if err != nil {
			return res, fmt.Errorf("%w: %q: %v", ErrImplement, cmd, err)
		}
		entry := repro.Result{Command: cmd, Code: r.Code, Output: text.Clip(r.Output, 4000)}
		switch {
		case r.OK():
			entry.Outcome = repro.Passed
		case toolchain.IsEnvironmentFailure(r.Code, r.Output):
			// Say so rather than blaming the patch: this repository cannot be
			// built here, which is a property of the repository.
			entry.Outcome, entry.Why = repro.Environment, "the repository could not be built in this container"
			res.EnvironmentFailure = true
		default:
			entry.Outcome, entry.Why = repro.Reproduced, fmt.Sprintf("exit %d", r.Code)
		}
		res.Tests = append(res.Tests, entry)
	}

	res.Verdict = Verify(ctx, j, c, diff)
	res.Blocked = blockers(res)
	res.OK = len(res.Blocked) == 0
	return res, nil
}

// blockers lists every reason this patch must not be submitted.
//
// Collected rather than short-circuited: one round of fixes should be able to
// clear all of them, and a report that names a single blocker at a time turns
// that into several.
func blockers(r Result) []string {
	var out []string
	if len(r.Verdict.Violates) > 0 {
		out = append(out, "contradicts a maintainer refusal: "+strings.Join(r.Verdict.Violates, "; "))
	}
	if len(r.Verdict.AIMentions) > 0 {
		out = append(out, "the diff mentions AI or tooling: "+strings.Join(r.Verdict.AIMentions, "; "))
	}
	if r.Verdict.TouchesWorkflows {
		out = append(out, "the diff modifies .github/ workflows")
	}
	if r.EnvironmentFailure {
		out = append(out, "the repository could not be built in the container, so nothing was verified")
	}
	// A new failure is the patch's fault; a pre-existing one is not, and the
	// caller supplies the baseline that separates them.
	for _, t := range r.Tests {
		if t.Outcome == repro.Reproduced {
			out = append(out, fmt.Sprintf("failing after the patch: %s", text.FirstLine(t.Command, 80)))
		}
	}
	return out
}

// NewFailures reports the tests this patch broke, ignoring what was already red.
func NewFailures(baseline []repro.Result, r Result) []string {
	return repro.NewFailures(baseline, r.Tests)
}

// Verify is the independent check that the patch honoured the refusals.
//
// A failure to run it is itself a blocker. An unverified patch and a verified
// one must never look the same from the outside, because the difference is the
// only thing standing between a maintainer refusal and a pull request that
// ignores it.
func Verify(ctx context.Context, j Judge, c *model.Candidate, diff string) Verdict {
	rejected := "(none stated)"
	if c.Brief != nil && len(c.Brief.RejectedApproaches) > 0 {
		lines := make([]string, len(c.Brief.RejectedApproaches))
		for i, r := range c.Brief.RejectedApproaches {
			lines[i] = "- " + r
		}
		rejected = strings.Join(lines, "\n")
	}
	if j == nil {
		return Verdict{Violates: []string{"no verifier available"}}
	}
	var v Verdict
	err := j.JudgeJSON(ctx, llm.Render(verifyPrompt, map[string]string{
		"rejected": rejected, "diff": text.Clip(diff, 40000),
	}), &v)
	if err != nil {
		return Verdict{Violates: []string{"verification step failed to run: " + err.Error()}}
	}
	return v
}

func changedFiles(ctx context.Context, g Git, clone string) []string {
	committed, pending := g.NameStatus(ctx, clone)
	seen := map[string]bool{}
	var out []string
	for _, block := range []string{committed, pending} {
		for _, line := range strings.Split(block, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			// `git diff --name-status` puts the status first; a rename puts
			// the destination last, which is the file that now exists.
			p := fields[len(fields)-1]
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}
