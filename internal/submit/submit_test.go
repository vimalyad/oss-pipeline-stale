package submit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fmt"
	"github.com/vimalyad/osspipeline/internal/guard"
	"github.com/vimalyad/osspipeline/internal/model"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
)

type fakeGit struct {
	replies  map[string]string
	errs     map[string]error
	ran      [][]string
	base     string
	topLevel map[string]bool
	style    string
}

func (f *fakeGit) Git(_ context.Context, _ string, args ...string) (string, error) {
	f.ran = append(f.ran, args)
	key := strings.Join(args, " ")
	for k, err := range f.errs {
		if strings.Contains(key, k) {
			return "", err
		}
	}
	for k, v := range f.replies {
		if strings.Contains(key, k) {
			return v, nil
		}
	}
	return "", nil
}
func (f *fakeGit) DefaultBranch(context.Context, string) string { return orElse(f.base, "main") }
func (f *fakeGit) TopLevelEntries(context.Context, string) map[string]bool {
	return f.topLevel
}
func (f *fakeGit) CommitStyle(context.Context, string) string {
	return orElse(f.style, "fix: subject")
}

func (f *fakeGit) didRun(sub string) bool {
	for _, a := range f.ran {
		if strings.Contains(strings.Join(a, " "), sub) {
			return true
		}
	}
	return false
}

type fakeJudge struct {
	out    string
	err    error
	stdin  string
	prompt string
}

func (f *fakeJudge) JudgeWith(_ context.Context, prompt, _, stdin string) (string, error) {
	f.prompt, f.stdin = prompt, stdin
	return f.out, f.err
}

type fakeGH struct {
	out  string
	err  error
	args []string
}

func (f *fakeGH) RESTRaw(_ context.Context, args []string, _ string) (string, error) {
	f.args = args
	return f.out, f.err
}

func candidate() *model.Candidate {
	return &model.Candidate{
		Repo: "kornia/kornia", Issue: 4201, Title: "MPS svd fails on Apple silicon",
		Branch: "fix/issue-4201",
	}
}

var ident = Identity{Name: "A Name", Email: "1+login@users.noreply.github.com",
	Login: "login", Private: []string{"work@example.com"}}

// TestBranchFilesSeesCommittedWork is the incident this package was rebuilt
// around: .agents/skills/kornia-developer/SKILL.md reached a public pull
// request because the check ran against uncommitted work only, and the file had
// already been committed.
func TestBranchFilesSeesCommittedWork(t *testing.T) {
	g := &fakeGit{replies: map[string]string{
		"diff --name-status origin/main...HEAD": "A\t.agents/skills/kornia/SKILL.md\nM\tkornia/x.py",
		"diff --name-status HEAD":               "",
		"ls-files --others":                     "",
	}}
	files, err := BranchFiles(context.Background(), g, "/clone")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(files, ".agents/skills/kornia/SKILL.md") {
		t.Fatalf("files = %v; a committed file is still shipped", files)
	}
}

// TestBranchFilesSeesUntrackedWork: commit runs `git add -A` and sweeps those
// in, so a file the implementer created but never committed still ships.
func TestBranchFilesSeesUntrackedWork(t *testing.T) {
	g := &fakeGit{replies: map[string]string{
		"diff --name-status origin/main...HEAD": "",
		"diff --name-status HEAD":               "",
		"ls-files --others":                     "notes-to-self.md\n.agents/plan.md\n",
	}}
	files, _ := BranchFiles(context.Background(), g, "/clone")
	if !contains(files, ".agents/plan.md") || !contains(files, "notes-to-self.md") {
		t.Fatalf("files = %v; untracked files are swept in by `git add -A`", files)
	}
}

func TestBranchFilesExcludesWhatTheTreeDeletes(t *testing.T) {
	g := &fakeGit{replies: map[string]string{
		"diff --name-status origin/main...HEAD": "A\tscratch.md\nM\tkornia/x.py",
		"diff --name-status HEAD":               "D\tscratch.md",
		"ls-files --others":                     "",
	}}
	files, _ := BranchFiles(context.Background(), g, "/clone")
	if contains(files, "scratch.md") {
		t.Fatalf("files = %v; a file the branch added and then removed is not shipped", files)
	}
	if !contains(files, "kornia/x.py") {
		t.Fatalf("files = %v", files)
	}
}

func TestBranchFilesHandlesRenames(t *testing.T) {
	g := &fakeGit{replies: map[string]string{
		"diff --name-status origin/main...HEAD": "R100\tkornia/old.py\tkornia/new.py",
		"diff --name-status HEAD":               "",
		"ls-files --others":                     "",
	}}
	files, _ := BranchFiles(context.Background(), g, "/clone")
	if !contains(files, "kornia/new.py") || contains(files, "kornia/old.py") {
		t.Fatalf("files = %v; the destination is the file that now exists", files)
	}
}

// TestBranchFilesFetchesFirst: a stale origin makes origin/main...HEAD include
// everything upstream merged since the clone, which for kornia was 250-plus
// files instead of three.
func TestBranchFilesFetchesFirst(t *testing.T) {
	g := &fakeGit{}
	if _, err := BranchFiles(context.Background(), g, "/clone"); err != nil {
		t.Fatal(err)
	}
	if !g.didRun("fetch -q origin main") {
		t.Fatalf("no fetch before diffing against origin: %v", g.ran)
	}
}

func TestPreflightCatchesEverySortOfProblem(t *testing.T) {
	g := &fakeGit{
		replies: map[string]string{
			"diff --name-status origin/main...HEAD": strings.Join([]string{
				"A\t.agents/skills/x/SKILL.md",
				"A\t.github/workflows/ci.yml",
				"A\tkornia/__pycache__/x.pyc",
				"A\tuv.lock",
				"A\tscratch/notes.md",
				"M\tkornia/real.py",
			}, "\n"),
			"diff --name-status HEAD": "",
			"ls-files --others":       "",
			"diff origin/main...HEAD": "diff --git a/x b/x\n+ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		},
		topLevel: map[string]bool{"kornia": true, "tests": true},
	}
	problems, err := Preflight(context.Background(), g, "/clone", ident)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.ToLower(problemText(problems))
	for _, want := range []string{
		"agent tooling", "workflow changes are excluded", "build artefact",
		"our own test tooling", "new top-level directory", "github token",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("preflight missed %q:\n%s", want, joined)
		}
	}
}

// TestPreflightScansForPrivateAddresses: the strings come from the gitignored
// identity file, never from this source, or making the repository public would
// publish the very address the check exists to suppress.
func TestPreflightScansForPrivateAddresses(t *testing.T) {
	g := &fakeGit{replies: map[string]string{
		"diff origin/main...HEAD": "diff --git a/x b/x\n+# contact WORK@example.com\n",
	}}
	problems, _ := Preflight(context.Background(), g, "/clone", ident)
	if !strings.Contains(problemText(problems), "private address") {
		t.Fatalf("a private address passed preflight: %v", problems)
	}
	// And with no private strings configured, nothing is invented.
	clean, _ := Preflight(context.Background(), g, "/clone", Identity{})
	if strings.Contains(problemText(clean), "private address") {
		t.Error("flagged a private address with none configured")
	}
}

func TestEmptyDiffIsAProblem(t *testing.T) {
	problems, _ := Preflight(context.Background(), &fakeGit{}, "/clone", ident)
	if !strings.Contains(problemText(problems), "empty diff") {
		t.Fatalf("problems = %v", problems)
	}
}

// TestCleanArtefactsLeavesTrackedFilesAlone: a repository that genuinely ships
// a uv.lock keeps it -- removing a tracked file would be a change to that
// project, not a cleanup of ours.
func TestCleanArtefactsLeavesTrackedFilesAlone(t *testing.T) {
	g := &fakeGit{replies: map[string]string{
		"ls-files":           "uv.lock\nkornia/x.py",
		"status --porcelain": "?? __pycache__/\n M uv.lock\n?? .DS_Store",
	}}
	removed, err := CleanArtefacts(context.Background(), g, "/clone")
	if err != nil {
		t.Fatal(err)
	}
	if contains(removed, "uv.lock") {
		t.Error("removed a file the project tracks")
	}
	if !contains(removed, "__pycache__/") || !contains(removed, ".DS_Store") {
		t.Fatalf("removed = %v", removed)
	}
}

func TestCommitMessageCarriesTheIssueAndStyle(t *testing.T) {
	j := &fakeJudge{out: "fix: guard the MPS element limit\n\nFixes #4201"}
	g := &fakeGit{style: "feat: x; fix: y", replies: map[string]string{"diff HEAD": "a diff"}}
	msg := CommitMessage(context.Background(), j, g, candidate(), "/clone", ident)
	if !strings.Contains(msg, "Fixes #4201") {
		t.Errorf("msg = %q", msg)
	}
	if !strings.Contains(j.prompt, "feat: x; fix: y") {
		t.Error("the repository's own commit style did not reach the prompt")
	}
	if j.stdin != "a diff" {
		t.Errorf("the diff went %q, want it on stdin", j.stdin)
	}
}

// TestCommitMessageFallsBackRatherThanBlocking: a dull subject is a small cost;
// abandoning a verified patch over a model call is not.
func TestCommitMessageFallsBackRatherThanBlocking(t *testing.T) {
	for _, j := range []Judge{&fakeJudge{err: errors.New("down")}, &fakeJudge{out: "  "}, nil} {
		msg := CommitMessage(context.Background(), j, &fakeGit{}, candidate(), "/clone", ident)
		if !strings.Contains(msg, "Fixes #4201") || msg == "" {
			t.Fatalf("msg = %q", msg)
		}
	}
}

// TestDCOSignOffOnlyWhenRequired, and disclosure never: git history stays clean
// whatever the project asks for.
func TestDCOSignOffOnlyWhenRequired(t *testing.T) {
	c := candidate()
	c.Facts = &model.RepoFacts{RequiresDCO: true, RequiresAIDisclosure: true}
	msg := CommitMessage(context.Background(), &fakeJudge{out: "fix: x"}, &fakeGit{}, c, "/clone", ident)
	if !strings.Contains(msg, "Signed-off-by: A Name <1+login@users.noreply.github.com>") {
		t.Errorf("msg = %q", msg)
	}
	if guard.BodyForbidden.MatchString(msg) {
		t.Errorf("the commit message mentions tooling: %q", msg)
	}

	c.Facts.RequiresDCO = false
	if msg := CommitMessage(context.Background(), &fakeJudge{out: "fix: x"}, &fakeGit{}, c, "/clone", ident); strings.Contains(msg, "Signed-off-by") {
		t.Errorf("signed off where the project does not ask: %q", msg)
	}
}

func TestFenceStripping(t *testing.T) {
	for in, want := range map[string]string{
		"```\nfix: x\n```":     "fix: x",
		"```text\nfix: x\n```": "fix: x",
		"fix: x":               "fix: x",
		"  fix: x  ":           "fix: x",
	} {
		if got := stripFence(in); got != want {
			t.Errorf("stripFence(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestALeakyBodyIsReplacedNotPublished: the implementer's closing message is
// addressed to the pipeline operator, and publishing it verbatim once put
// first-person notes about sandbox denials on a real pull request.
func TestALeakyBodyIsReplacedNotPublished(t *testing.T) {
	c := candidate()
	c.Brief = &model.Brief{AcceptanceCriteria: []string{"the MPS test passes"}}
	leaky := "I was unable to run the tests because the sandbox denied the command."

	body := ComposeBody(context.Background(), &fakeJudge{out: leaky}, &fakeGit{}, c, "/clone", "pytest")
	if strings.Contains(body, "sandbox") || strings.Contains(body, "I was unable") {
		t.Fatalf("a leaky body was published:\n%s", body)
	}
	if !strings.Contains(body, "Addresses #4201") || !strings.Contains(body, "the MPS test passes") {
		t.Errorf("the fallback is not usable:\n%s", body)
	}
	if !strings.Contains(body, "Verification: pytest") {
		t.Errorf("body = %q", body)
	}
}

func TestComposeBodyFetchesAndUsesTheBranchDiff(t *testing.T) {
	g := &fakeGit{replies: map[string]string{"diff origin/main...HEAD": "the branch diff"}}
	j := &fakeJudge{out: "A clear description of the change."}
	ComposeBody(context.Background(), j, g, candidate(), "/clone", "pytest")
	if !g.didRun("fetch -q origin main") {
		t.Error("composed a body from a stale origin")
	}
	if j.stdin != "the branch diff" {
		t.Errorf("stdin = %q; the body must come from the branch diff", j.stdin)
	}
}

// TestTitleDescribesTheChangeNotTheBug: prefixing the issue title produced
// "fix: MPS: torch 2.14 ... fail at >= 8192 input elements" -- doubled colon,
// truncated, and about the symptom.
func TestTitleDescribesTheChangeNotTheBug(t *testing.T) {
	g := &fakeGit{replies: map[string]string{"log -1": "docs: clarify the MPS element limit"}}
	if got := Title(context.Background(), g, candidate(), "/clone"); got != "docs: clarify the MPS element limit" {
		t.Fatalf("title = %q, want the commit subject", got)
	}
	// Only with no commit does the issue title stand in.
	if got := Title(context.Background(), &fakeGit{}, candidate(), "/clone"); !strings.HasPrefix(got, "fix: MPS svd fails") {
		t.Fatalf("fallback title = %q", got)
	}
}

func TestBodyAppendsFixesAndCredits(t *testing.T) {
	c := candidate()
	c.Contest = model.ContestStalePR
	c.PRSignal = &model.PRSignal{Number: 77, Author: "earlier-person"}
	body := Body(c, "A description.")
	if !strings.Contains(body, "Fixes #4201") {
		t.Error("no Fixes line")
	}
	if !strings.Contains(body, "builds on the earlier work in #77 by @earlier-person") {
		t.Errorf("no credit:\n%s", body)
	}
	if c.Credits != "earlier-person" {
		t.Errorf("credits = %q; the ledger reads this later", c.Credits)
	}
}

// TestDisclosureOnlyWhenTheProjectAsks, and only in the body.
func TestDisclosureOnlyWhenTheProjectAsks(t *testing.T) {
	c := candidate()
	if b := Body(c, "d"); strings.Contains(b, "AI assistance") {
		t.Error("disclosed unprompted")
	}
	c.Facts = &model.RepoFacts{RequiresAIDisclosure: true}
	b := Body(c, "d")
	if !strings.Contains(b, "prepared with AI assistance") {
		t.Errorf("no disclosure where the project asks:\n%s", b)
	}
	if !c.DisclosedAI {
		t.Error("the disclosure was not recorded on the candidate")
	}
}

// TestOpenRechecksTheBody: everything between composing it and pushing is a
// chance for it to have changed, and this is the last moment a leak is private.
func TestOpenRechecksTheBody(t *testing.T) {
	gh := &fakeGH{out: "https://github.com/kornia/kornia/pull/4455"}
	g := &fakeGit{}
	p := Plan{Title: "t", Body: "Prepared by Claude.", Branch: "b", Base: "main", Head: "login:b"}
	if _, err := Open(context.Background(), g, gh, candidate(), "/clone", p); !errors.Is(err, ErrSubmit) {
		t.Fatalf("err = %v", err)
	}
	if gh.args != nil {
		t.Fatal("a leaky body reached gh pr create")
	}
	if g.didRun("push") {
		t.Fatal("pushed despite refusing to open the pull request")
	}
}

func TestOpenRecordsTheURLAndNumber(t *testing.T) {
	gh := &fakeGH{out: "https://github.com/kornia/kornia/pull/4455\n"}
	c := candidate()
	p := Plan{Title: "docs: x", Body: "A description.\n\nFixes #4201\n",
		Branch: "fix/issue-4201", Base: "main", Head: "login:fix/issue-4201"}

	url, err := Open(context.Background(), &fakeGit{}, gh, c, "/clone", p)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/kornia/kornia/pull/4455" || c.PRNumber == nil || *c.PRNumber != 4455 {
		t.Fatalf("url=%q number=%v", url, c.PRNumber)
	}
	joined := strings.Join(gh.args, " ")
	for _, want := range []string{"pr create", "--repo kornia/kornia", "--base main",
		"--head login:fix/issue-4201", "--title docs: x"} {
		if !strings.Contains(joined, want) {
			t.Errorf("gh args missing %q: %v", want, gh.args)
		}
	}
}

func TestOpenRefusesWithoutABranch(t *testing.T) {
	gh := &fakeGH{}
	p := Plan{Title: "t", Body: "clean text", Base: "main"}
	if _, err := Open(context.Background(), &fakeGit{}, gh, candidate(), "/clone", p); !errors.Is(err, ErrSubmit) {
		t.Fatalf("err = %v", err)
	}
	if gh.args != nil {
		t.Fatal("opened a pull request with no branch")
	}
}

func TestAFailedPushDoesNotOpenAPullRequest(t *testing.T) {
	gh := &fakeGH{}
	g := &fakeGit{errs: map[string]error{"push": errors.New("rejected: non-fast-forward")}}
	p := Plan{Title: "t", Body: "clean text", Branch: "b", Base: "main", Head: "login:b"}
	if _, err := Open(context.Background(), g, gh, candidate(), "/clone", p); !errors.Is(err, ErrSubmit) {
		t.Fatalf("err = %v", err)
	}
	if gh.args != nil {
		t.Fatal("opened a pull request for a branch that was never pushed")
	}
}

func TestPrepareProducesAPlanWithoutTouchingGitHub(t *testing.T) {
	gh := &fakeGH{}
	p := Prepare(context.Background(), &fakeJudge{out: "A clear description."},
		&fakeGit{replies: map[string]string{"log -1": "fix: the thing"}},
		candidate(), "/clone", "pytest -q", ident)
	if p.Title != "fix: the thing" || p.Head != "login:fix/issue-4201" || p.Base != "main" {
		t.Fatalf("plan = %+v", p)
	}
	if !strings.Contains(p.Body, "Fixes #4201") {
		t.Errorf("body = %q", p.Body)
	}
	if gh.args != nil {
		t.Fatal("Prepare called GitHub")
	}
}

func problemText(ps []guard.Problem) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Why)
	}
	return strings.Join(out, "\n")
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestOnlySubmitPublishes scans the source for the two operations that make
// something public: pushing a branch, and opening a pull request.
//
// Both must live here, behind Preflight and behind the body re-check. A second
// site anywhere else would be a way for a branch to reach a maintainer without
// passing the scan that exists to stop agent scaffolding, a leaked credential
// or an agent transcript going out under the user's name -- and it would be the
// kind of thing nobody notices until it has already happened once.
func TestOnlySubmitPublishes(t *testing.T) {
	fset := token.NewFileSet()
	offenders := map[string][]string{}

	// Only this project's own source. work/ holds clones of the repositories
	// being contributed to, and gh's own source naturally opens pull requests.
	var err error
	for _, tree := range []string{filepath.Join("..", "..", "internal"), filepath.Join("..", "..", "cmd")} {
		err = filepath.Walk(tree, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return err
			}
			slash := filepath.ToSlash(path)
			if strings.Contains(slash, "internal/submit/") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var literals []string
				for _, a := range call.Args {
					if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						literals = append(literals, strings.Trim(lit.Value, `"`))
					}
				}
				joined := strings.Join(literals, " ")
				// A bare "push" argument to any call, or the gh pr-create pair.
				for _, l := range literals {
					if l == "push" {
						offenders[slash] = append(offenders[slash],
							fmt.Sprintf("line %d: pushes a branch", fset.Position(call.Pos()).Line))
					}
				}
				if strings.Contains(joined, "pr") && strings.Contains(joined, "create") {
					offenders[slash] = append(offenders[slash],
						fmt.Sprintf("line %d: opens a pull request", fset.Position(call.Pos()).Line))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		var lines []string
		for f, ls := range offenders {
			lines = append(lines, f+": "+strings.Join(ls, "; "))
		}
		sort.Strings(lines)
		t.Fatalf("something outside internal/submit publishes:\n  %s\n"+
			"every branch must reach a maintainer through Preflight and the body re-check",
			strings.Join(lines, "\n  "))
	}
}
