// Package submit commits, pushes and opens the pull request.
//
// This is the last gate before anything is public, and the order is the point:
// identity is asserted, the branch's whole shipment is scanned for secrets and
// for files we promised not to touch, and only then is a commit created. The
// commit-msg and pre-push hooks fire underneath as an independent backstop.
//
// Two incidents shaped most of what is here. Agent scaffolding reached a real
// kornia pull request because the check ran against uncommitted work only, and
// the file had already been committed. And the implementer's closing message --
// addressed to the pipeline operator, not to maintainers -- was published
// verbatim as a pull request body, putting first-person notes about sandbox
// denials and a guessed pull request number in front of a maintainer.
package submit

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vimalyad/osspipeline/internal/guard"
	"github.com/vimalyad/osspipeline/internal/llm"
	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/text"
)

//go:embed prompts/commit.md
var commitPrompt string

//go:embed prompts/body.md
var bodyPrompt string

var ErrSubmit = errors.New("submit")

// Git is the repository surface this package needs. Everything goes through it
// so every command carries the isolated account's identity environment; a call
// that shelled out to git directly would silently use the machine's default
// account, which is the failure identity isolation exists to prevent.
type Git interface {
	Git(ctx context.Context, dir string, args ...string) (string, error)
	DefaultBranch(ctx context.Context, dir string) string
	TopLevelEntries(ctx context.Context, dir string) map[string]bool
	CommitStyle(ctx context.Context, dir string) string
}

// Judge writes the commit message and the pull request body.
type Judge interface {
	JudgeWith(ctx context.Context, prompt, system, stdin string) (string, error)
}

// GH opens the pull request.
type GH interface {
	RESTRaw(ctx context.Context, args []string, stdin string) (string, error)
}

// Identity is the account everything is published as.
type Identity struct {
	Name    string
	Email   string
	Login   string
	Private []string
}

const maxDiffForPrompt = 60000

// BranchFiles is every file this branch will actually ship.
//
// Not `git diff HEAD`, which is uncommitted work only. Checking that scope is
// how .agents/skills/kornia-developer/SKILL.md reached a public pull request:
// the rule matched the path perfectly, but the file had already been committed,
// so the check ran against an empty set and reported clean.
//
// Untracked files count as shipped too, because commit runs `git add -A` and
// sweeps exactly those in -- the same shape of mistake, one scope further out.
func BranchFiles(ctx context.Context, g Git, dir string) ([]string, error) {
	base := g.DefaultBranch(ctx, dir)
	// Fetch first. A stale origin makes origin/base...HEAD include everything
	// upstream merged since the clone, which for kornia was 250-plus files
	// instead of three.
	_, _ = g.Git(ctx, dir, "fetch", "-q", "origin", base)

	committed := map[string]bool{}
	out, _ := g.Git(ctx, dir, "diff", "--name-status", "origin/"+base+"...HEAD")
	for _, path := range statusPaths(out, "AMRC") {
		committed[path] = true
	}

	add, del := map[string]bool{}, map[string]bool{}
	pending, _ := g.Git(ctx, dir, "diff", "--name-status", "HEAD")
	for _, line := range strings.Split(pending, "\n") {
		st, path, ok := splitStatus(line)
		if !ok {
			continue
		}
		if st == 'D' {
			del[path] = true
		} else {
			add[path] = true
		}
	}

	untracked, _ := g.Git(ctx, dir, "ls-files", "--others", "--exclude-standard")
	for _, p := range strings.Split(untracked, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			add[p] = true
		}
	}

	set := map[string]bool{}
	for p := range committed {
		set[p] = true
	}
	for p := range add {
		set[p] = true
	}
	var files []string
	for p := range set {
		if !del[p] {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	return files, nil
}

// Preflight is everything that must be true before this branch becomes a pull
// request. It returns every problem, not the first: one round of cleanup should
// clear them all.
func Preflight(ctx context.Context, g Git, dir string, id Identity) ([]guard.Problem, error) {
	files, err := BranchFiles(ctx, g, dir)
	if err != nil {
		return nil, err
	}
	base := g.DefaultBranch(ctx, dir)
	branchDiff, _ := g.Git(ctx, dir, "diff", "origin/"+base+"...HEAD")
	workingDiff, _ := g.Git(ctx, dir, "diff", "HEAD")

	return guard.Inspect(guard.Shipment{
		Files:            files,
		Diff:             branchDiff + "\n" + workingDiff,
		ExistingTopLevel: g.TopLevelEntries(ctx, dir),
		Private:          id.Private,
	}), nil
}

// CleanArtefacts drops build output and our own tooling's leavings before
// committing.
//
// Only untracked paths. A repository that genuinely ships a uv.lock keeps it --
// removing a tracked file would be a change to that project, not a cleanup of
// ours.
func CleanArtefacts(ctx context.Context, g Git, dir string) ([]string, error) {
	tracked := map[string]bool{}
	out, _ := g.Git(ctx, dir, "ls-files")
	for _, p := range strings.Split(out, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			tracked[p] = true
		}
	}
	status, _ := g.Git(ctx, dir, "status", "--porcelain")
	var removed []string
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if path == "" || tracked[path] {
			continue
		}
		if !guard.Junk.MatchString(path) && !guard.OurTooling[path] {
			continue
		}
		if _, err := g.Git(ctx, dir, "clean", "-fdx", "--", path); err != nil {
			continue
		}
		removed = append(removed, path)
	}
	sort.Strings(removed)
	return removed, nil
}

var fenceRe = regexp.MustCompile("(?s)^```[a-zA-Z]*\n(.*)\n```$")

func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if m := fenceRe.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return s
}

// CommitMessage writes the message for the staged change.
//
// A failure falls back to something plain and true rather than blocking the
// submission: a dull commit subject is a small cost, and the alternative is
// abandoning a verified patch over a model call.
func CommitMessage(ctx context.Context, j Judge, g Git, c *model.Candidate, dir string, id Identity) string {
	diff, _ := g.Git(ctx, dir, "diff", "HEAD")
	prompt := llm.Render(commitPrompt, map[string]string{
		"style":     g.CommitStyle(ctx, dir),
		"issue_ref": fmt.Sprintf("Fixes #%d", c.Issue),
	})
	msg := ""
	if j != nil {
		if out, err := j.JudgeWith(ctx, prompt, "", text.Clip(diff, maxDiffForPrompt)); err == nil {
			msg = stripFence(out)
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("fix: address issue #%d\n\nFixes #%d", c.Issue, c.Issue)
	}
	// Disclosure never enters git history -- that is a standing rule and holds
	// regardless of what the project asks for. A DCO sign-off is different:
	// it is the project's own requirement on the commit itself.
	if c.Facts != nil && c.Facts.RequiresDCO {
		msg += fmt.Sprintf("\n\nSigned-off-by: %s <%s>", id.Name, id.Email)
	}
	return msg
}

// Commit stages everything and commits. The hooks in the clone fire here and
// are the independent backstop to Preflight.
func Commit(ctx context.Context, g Git, dir, message string) error {
	if _, err := g.Git(ctx, dir, "add", "-A"); err != nil {
		return fmt.Errorf("%w: %v", ErrSubmit, err)
	}
	if _, err := g.Git(ctx, dir, "commit", "-m", message); err != nil {
		return fmt.Errorf("%w: commit rejected: %v", ErrSubmit, err)
	}
	return nil
}

// ComposeBody writes the pull request description from the diff.
//
// Never from the implementer's summary. That message is addressed to the
// pipeline operator, and publishing it verbatim put an agent transcript on a
// real pull request: first-person notes about sandbox denials, a claim the
// tests had not been run when the pipeline had since run them, and an admission
// of guessing a pull request number.
func ComposeBody(ctx context.Context, j Judge, g Git, c *model.Candidate, dir, verification string) string {
	base := g.DefaultBranch(ctx, dir)
	_, _ = g.Git(ctx, dir, "fetch", "-q", "origin", base)
	// Committed work and uncommitted work both. A dry run has not committed
	// anything, so asking only for origin/base...HEAD hands the model an empty
	// string -- and a model given nothing to describe answers the operator
	// instead: "I don't see any diff content in your message ... Could you
	// paste the diff itself?" That reply was then composed into a pull request
	// body. Nothing downstream can rescue a prompt that contained no change.
	committed, _ := g.Git(ctx, dir, "diff", "origin/"+base+"...HEAD")
	working, _ := g.Git(ctx, dir, "diff", "HEAD")
	diff := strings.TrimSpace(committed + "\n" + working)
	if diff == "" {
		return fallbackBody(c, verification)
	}

	prompt := llm.Render(bodyPrompt, map[string]string{
		"repo": c.Repo, "issue": strconv.Itoa(c.Issue), "title": c.Title,
		"verification": orElse(verification, "the project's own tests"),
	})
	body := ""
	if j != nil {
		if out, err := j.JudgeWith(ctx, prompt, "", text.Clip(diff, maxDiffForPrompt)); err == nil {
			body = stripFence(out)
		}
	}
	if bad := guard.CheckBody(body); body == "" || len(bad) > 0 {
		// Fall back to something plain and true rather than publish a leak.
		return fallbackBody(c, verification)
	}
	return body
}

func fallbackBody(c *model.Candidate, verification string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Addresses #%d (%s).\n\n", c.Issue, c.Title)
	if c.Brief != nil {
		for _, crit := range c.Brief.AcceptanceCriteria {
			fmt.Fprintf(&b, "- %s\n", crit)
		}
		if len(c.Brief.AcceptanceCriteria) > 0 {
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "Verification: %s.", orElse(verification, "the project's own tests"))
	return b.String()
}

// Title is what a maintainer reads first, so it describes the CHANGE.
//
// The issue title describes the BUG. Prefixing it produced things like
// "fix: MPS: torch 2.14 ... fail at >= 8192 input elements" -- a doubled colon,
// truncated, and about the symptom rather than the fix. The commit subject just
// written is the right summary; the issue title is only the fallback for when
// there is no commit yet.
func Title(ctx context.Context, g Git, c *model.Candidate, dir string) string {
	// Only a commit this branch made. A dry run has committed nothing, so HEAD
	// is still upstream's -- and reading its subject produced the title
	// "chore(deps): bump the github-actions group with 4 updates (#32677)" for
	// a symlink fix. The count is what distinguishes our work from theirs.
	base := g.DefaultBranch(ctx, dir)
	if out, err := g.Git(ctx, dir, "rev-list", "--count", "origin/"+base+"..HEAD"); err == nil {
		if n, cerr := strconv.Atoi(strings.TrimSpace(out)); cerr == nil && n > 0 {
			if subject, err := g.Git(ctx, dir, "log", "-1", "--format=%s"); err == nil {
				if s := strings.TrimSpace(subject); s != "" {
					return s
				}
			}
		}
	}
	return "fix: " + text.Clip(c.Title, 70)
}

// Body assembles the full pull request description.
func Body(c *model.Candidate, summary string) string {
	var b strings.Builder
	b.WriteString(summary)
	b.WriteString("\n\n")

	// A takeover credits the earlier author in the body, and records it on the
	// candidate so the ledger can show it later.
	if c.Contest == model.ContestStalePR && c.PRSignal != nil {
		// Two wordings, because only one of them is true at a time. Claiming
		// the earlier commits are preserved when the branch was cut fresh from
		// upstream tells a maintainer something they can check and find false,
		// which is worse than not crediting at all.
		if c.TookOver {
			fmt.Fprintf(&b, "This builds on the earlier work in #%d by @%s; "+
				"their commits are preserved in the history.\n\n",
				c.PRSignal.Number, c.PRSignal.Author)
		} else {
			fmt.Fprintf(&b, "#%d by @%s covered the same ground; this is a fresh "+
				"branch, but the approach there informed it.\n\n",
				c.PRSignal.Number, c.PRSignal.Author)
		}
		c.Credits = c.PRSignal.Author
	}
	fmt.Fprintf(&b, "Fixes #%d\n", c.Issue)

	// Disclosure goes in the pull request body only when the project asks for
	// it. Git history stays clean either way -- that is the standing rule.
	if c.Facts != nil && c.Facts.RequiresAIDisclosure {
		b.WriteString("\n---\n_Per this project's contributing guidelines: this change " +
			"was prepared with AI assistance and reviewed by me before submission._\n")
		c.DisclosedAI = true
	}
	return b.String()
}

// Plan is what a dry run reports and an execute run performs.
type Plan struct {
	Title  string
	Body   string
	Branch string
	Base   string
	Head   string
}

// Prepare builds the pull request without opening it.
func Prepare(ctx context.Context, j Judge, g Git, c *model.Candidate, dir, verification string, id Identity) Plan {
	base := g.DefaultBranch(ctx, dir)
	return Plan{
		Title:  Title(ctx, g, c, dir),
		Body:   Body(c, ComposeBody(ctx, j, g, c, dir, verification)),
		Branch: c.Branch,
		Base:   base,
		Head:   id.Login + ":" + c.Branch,
	}
}

// Open pushes the branch and opens the pull request.
//
// The body is re-checked immediately before it is sent. Everything between
// composing it and here is a chance for it to have changed, and this is the
// last moment at which a leak is still private.
func Open(ctx context.Context, g Git, gh GH, c *model.Candidate, dir string, p Plan) (string, error) {
	if bad := guard.CheckBody(p.Body); len(bad) > 0 {
		return "", fmt.Errorf("%w: the pull request body mentions %s",
			ErrSubmit, strings.Join(bad, ", "))
	}
	if strings.TrimSpace(p.Branch) == "" {
		return "", fmt.Errorf("%w: no branch to push", ErrSubmit)
	}
	if _, err := g.Git(ctx, dir, "push", "--set-upstream", "fork", p.Branch); err != nil {
		return "", fmt.Errorf("%w: push: %v", ErrSubmit, err)
	}
	out, err := gh.RESTRaw(ctx, []string{
		"pr", "create", "--repo", c.Repo, "--base", p.Base, "--head", p.Head,
		"--title", p.Title, "--body", p.Body,
	}, "")
	if err != nil {
		return "", fmt.Errorf("%w: gh pr create: %v", ErrSubmit, err)
	}
	url := lastLine(out)
	n, perr := prNumber(url)
	if perr != nil {
		return url, fmt.Errorf("%w: opened %s but could not read its number: %v", ErrSubmit, url, perr)
	}
	c.PRURL, c.PRNumber = url, &n
	return url, nil
}

func prNumber(url string) (int, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(url), "/")
	i := strings.LastIndexByte(trimmed, '/')
	if i < 0 {
		return 0, fmt.Errorf("%q is not a pull request URL", url)
	}
	return strconv.Atoi(trimmed[i+1:])
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func statusPaths(out, statuses string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		st, path, ok := splitStatus(line)
		if ok && strings.IndexByte(statuses, st) >= 0 {
			paths = append(paths, path)
		}
	}
	return paths
}

// splitStatus reads one `git diff --name-status` line. A rename or copy has
// two paths; the last is the file that now exists, which is the one shipping.
func splitStatus(line string) (byte, string, bool) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return 0, "", false
	}
	fields := strings.Split(line, "\t")
	if len(fields) < 2 || fields[0] == "" {
		return 0, "", false
	}
	return fields[0][0], strings.TrimSpace(fields[len(fields)-1]), true
}

func orElse(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
