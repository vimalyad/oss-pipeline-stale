package implement

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/repro"
	"github.com/vimalyad/osspipeline/internal/sandbox"
)

type fakeAgent struct {
	out    string
	err    error
	prompt string
	clone  string
	edits  map[string]string // written into the clone when Patch runs
}

func (f *fakeAgent) Patch(_ context.Context, clone, prompt string) (string, error) {
	f.prompt, f.clone = prompt, clone
	for p, body := range f.edits {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(clone, p)), 0o755)
		_ = os.WriteFile(filepath.Join(clone, p), []byte(body), 0o644)
	}
	return f.out, f.err
}

type fakeJudge struct {
	v      Verdict
	err    error
	prompt string
}

func (f *fakeJudge) JudgeJSON(_ context.Context, prompt string, v any) error {
	f.prompt = prompt
	if f.err != nil {
		return f.err
	}
	b, _ := json.Marshal(f.v)
	return json.Unmarshal(b, v)
}

type fakeGit struct {
	diff      string
	committed string
	pending   string
}

func (f *fakeGit) Diff(context.Context, string) string { return f.diff }
func (f *fakeGit) NameStatus(context.Context, string) (string, string) {
	return f.committed, f.pending
}

type fakeRunner struct {
	replies map[string]sandbox.Result
	ran     []string
	err     error
}

func (f *fakeRunner) Run(_ context.Context, cmd string) (sandbox.Result, error) {
	f.ran = append(f.ran, cmd)
	if f.err != nil {
		return sandbox.Result{}, f.err
	}
	for k, v := range f.replies {
		if strings.Contains(cmd, k) {
			v.Command = cmd
			return v, nil
		}
	}
	return sandbox.Result{Code: 0, Command: cmd}, nil
}

func candidate() *model.Candidate {
	return &model.Candidate{
		Repo: "kornia/kornia", Issue: 4201, Title: "MPS svd fails",
		URL: "https://github.com/kornia/kornia/issues/4201",
		Brief: &model.Brief{
			MaintainerDesiredApproach: "document the limit and raise a clear error",
			ApproachAuthorAssociation: "OWNER",
			ApproachSourceURL:         "https://c2",
			RejectedApproaches:        []string{"do not cast to float64 in the hot path"},
			AcceptanceCriteria:        []string{"the MPS test passes"},
			Reproduction:              "run pytest tests/geometry",
		},
	}
}

func pythonClone(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range map[string]string{
		"pyproject.toml":           "[project]\nname='k'\n",
		"kornia/core/utils.py":     "def check(): pass\n",
		"tests/core/test_utils.py": "def test_check(): pass\n",
	} {
		full := filepath.Join(root, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestTheSpecReachesThePromptAsInstructions: the brief is the spec, not a
// suggestion, and a refusal has to arrive as a prohibition rather than as
// background reading.
func TestTheSpecReachesThePromptAsInstructions(t *testing.T) {
	got, err := Prompt(candidate(), []string{"pytest -q"}, "the issue body")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kornia/kornia#4201", "MPS svd fails", "the issue body",
		`MAINTAINER WANTS (OWNER): "document the limit and raise a clear error"`,
		"MUST NOT: do not cast to float64 in the hot path",
		"DONE WHEN: the MPS test passes",
		"REPRODUCTION: run pytest tests/geometry",
		"  pytest -q",
		"You have no shell",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	if strings.Contains(got, "{{") {
		t.Error("an unrendered placeholder reached the prompt")
	}
}

// TestNoBriefIsARefusal: a patch written from the issue title alone is a guess,
// and a guess costs a maintainer their review time.
func TestNoBriefIsARefusal(t *testing.T) {
	c := candidate()
	c.Brief = nil
	if _, err := Prompt(c, nil, ""); !errors.Is(err, ErrNoBrief) {
		t.Fatalf("err = %v, want ErrNoBrief", err)
	}
	c.Brief = &model.Brief{} // present but empty
	if _, err := Prompt(c, nil, ""); !errors.Is(err, ErrNoBrief) {
		t.Fatalf("empty brief: err = %v", err)
	}
}

// TestAnEmptyDiffKeepsTheHeadOfTheRefusal: a refusal leads with its conclusion,
// and truncating from the end once left a fragment of a sentence as the entire
// recorded reason for declining a real candidate.
func TestAnEmptyDiffKeepsTheHeadOfTheRefusal(t *testing.T) {
	reason := "This cannot be fixed as specified because the API was removed upstream. " +
		strings.Repeat("Further detail. ", 500)
	a := &fakeAgent{out: reason}
	res, err := Run(context.Background(), a, &fakeJudge{}, &fakeGit{diff: "  \n"},
		&fakeRunner{}, &fakeRunner{}, candidate(), pythonClone(t), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DiffEmpty || res.OK {
		t.Fatalf("res = %+v", res)
	}
	if !strings.HasPrefix(res.Refusal, "This cannot be fixed as specified") {
		t.Errorf("refusal starts %q; the conclusion is at the front", res.Refusal[:60])
	}
}

// TestVerificationFailureBlocks: an unverified patch and a verified one must
// never look the same, because the difference is the only thing between a
// maintainer refusal and a pull request that ignores it.
func TestVerificationFailureBlocks(t *testing.T) {
	res, err := Run(context.Background(), &fakeAgent{out: "done"}, &fakeJudge{err: errors.New("claude down")},
		&fakeGit{diff: "diff --git a/x b/x\n+1"}, &fakeRunner{}, &fakeRunner{},
		candidate(), pythonClone(t), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("an unverified patch was marked OK")
	}
	if len(res.Blocked) == 0 || !strings.Contains(strings.Join(res.Blocked, " "), "verification") {
		t.Errorf("blocked = %v", res.Blocked)
	}
}

func TestEveryVerdictProblemBlocks(t *testing.T) {
	tests := []struct {
		name string
		v    Verdict
		want string
	}{
		{"a refusal was contradicted", Verdict{Violates: []string{"casts to float64"}}, "maintainer refusal"},
		{"the diff mentions AI", Verdict{AIMentions: []string{"generated by Claude"}}, "mentions AI"},
		{"workflows were touched", Verdict{TouchesWorkflows: true}, ".github/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Run(context.Background(), &fakeAgent{out: "done"}, &fakeJudge{v: tt.v},
				&fakeGit{diff: "diff --git a/x b/x\n+1"}, &fakeRunner{}, &fakeRunner{},
				candidate(), pythonClone(t), nil, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if res.OK {
				t.Fatal("marked OK")
			}
			if !strings.Contains(strings.Join(res.Blocked, " "), tt.want) {
				t.Errorf("blocked = %v, want one mentioning %q", res.Blocked, tt.want)
			}
		})
	}
}

// TestBlockersAccumulate: one round of fixes should clear all of them, and a
// report naming a single blocker at a time turns that into several.
func TestBlockersAccumulate(t *testing.T) {
	res, err := Run(context.Background(), &fakeAgent{out: "done"},
		&fakeJudge{v: Verdict{Violates: []string{"x"}, AIMentions: []string{"y"}, TouchesWorkflows: true}},
		&fakeGit{diff: "diff --git a/x b/x\n+1"}, &fakeRunner{}, &fakeRunner{},
		candidate(), pythonClone(t), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Blocked) < 3 {
		t.Fatalf("blocked = %v, want all three", res.Blocked)
	}
}

// TestAnUnbuildableRepoIsNotABadPatch: one repository declares no dependencies
// and compiles a C++ extension in CI, so importing it from source fails however
// good the diff is. Calling that a bad patch discards correct work.
func TestAnUnbuildableRepoIsNotABadPatch(t *testing.T) {
	offline := &fakeRunner{replies: map[string]sandbox.Result{
		"pytest": {Code: 1, Output: "ModuleNotFoundError: No module named 'kornia_rs'"},
	}}
	res, err := Run(context.Background(), &fakeAgent{out: "done"}, &fakeJudge{},
		&fakeGit{diff: "diff --git a/x b/x\n+1", pending: "M\tkornia/core/utils.py"},
		&fakeRunner{}, offline, candidate(), pythonClone(t), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.EnvironmentFailure {
		t.Fatal("an unbuildable container was blamed on the diff")
	}
	var envSeen bool
	for _, tr := range res.Tests {
		if tr.Outcome == repro.Environment {
			envSeen = true
		}
		if tr.Outcome == repro.Reproduced {
			t.Errorf("an environment failure was recorded as a failing test: %+v", tr)
		}
	}
	if !envSeen {
		t.Error("no environment verdict recorded")
	}
	if res.OK {
		t.Error("marked OK despite verifying nothing")
	}
}

// TestInstallUsesTheNetworkedSessionAndTestsDoNot is the property that makes
// the offline run an oracle: a test passing only because it reached the
// internet has verified nothing.
func TestInstallUsesTheNetworkedSessionAndTestsDoNot(t *testing.T) {
	net, offline := &fakeRunner{}, &fakeRunner{}
	clone := pythonClone(t)
	_, err := Run(context.Background(), &fakeAgent{out: "done"}, &fakeJudge{},
		&fakeGit{diff: "diff --git a/x b/x\n+1", pending: "M\tkornia/core/utils.py"},
		net, offline, candidate(), clone, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range net.ran {
		if strings.Contains(cmd, "pytest") {
			t.Errorf("a test ran in the networked session: %q", cmd)
		}
	}
	if len(offline.ran) == 0 {
		t.Fatal("nothing ran in the offline session")
	}
	joined := strings.Join(offline.ran, " ")
	if !strings.Contains(joined, "pytest") {
		t.Errorf("offline session ran %v", offline.ran)
	}
}

func TestTargetedScopeIsReported(t *testing.T) {
	clone := pythonClone(t)
	res, err := Run(context.Background(), &fakeAgent{out: "done"}, &fakeJudge{},
		&fakeGit{diff: "diff --git a b\n+1", pending: "M\tkornia/core/utils.py"},
		&fakeRunner{}, &fakeRunner{}, candidate(), clone, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TestScope == "" {
		t.Fatal("no rationale recorded; a report cannot say what was actually run")
	}
}

// TestNewFailuresIgnoreWhatWasAlreadyRed: without a baseline, a container that
// disagrees with the maintainer's CI makes every pre-existing red test look
// like something this change broke.
func TestNewFailuresIgnoreWhatWasAlreadyRed(t *testing.T) {
	baseline := []repro.Result{{Outcome: repro.Reproduced,
		Output: "FAILED tests/geometry/test_homography.py::test_clean_points - nan\n"}}
	res := Result{Tests: []repro.Result{{Outcome: repro.Reproduced,
		Output: "FAILED tests/geometry/test_homography.py::test_clean_points - nan\n" +
			"FAILED tests/core/test_utils.py::test_new - AssertionError\n"}}}

	got := NewFailures(baseline, res)
	if len(got) != 1 || got[0] != "tests/core/test_utils.py::test_new" {
		t.Fatalf("= %v, want only the new failure", got)
	}
}

func TestVerifyPromptCarriesTheRefusalsAndTheDiff(t *testing.T) {
	j := &fakeJudge{}
	Verify(context.Background(), j, candidate(), "diff --git a/kornia/x.py b/kornia/x.py")
	for _, want := range []string{"- do not cast to float64 in the hot path",
		"diff --git a/kornia/x.py"} {
		if !strings.Contains(j.prompt, want) {
			t.Errorf("verify prompt is missing %q", want)
		}
	}
	// No refusals must read as "none stated", not as an empty list the model
	// might fill in.
	c := candidate()
	c.Brief.RejectedApproaches = nil
	j2 := &fakeJudge{}
	Verify(context.Background(), j2, c, "d")
	if !strings.Contains(j2.prompt, "(none stated)") {
		t.Error("an absent refusal list rendered blank")
	}
}

func TestChangedFilesHandlesRenames(t *testing.T) {
	g := &fakeGit{
		committed: "M\tkornia/a.py\nR100\tkornia/old.py\tkornia/new.py",
		pending:   "A\ttests/test_new.py\nM\tkornia/a.py",
	}
	got := changedFiles(context.Background(), g, "/x")
	want := map[string]bool{"kornia/a.py": true, "kornia/new.py": true, "tests/test_new.py": true}
	if len(got) != len(want) {
		t.Fatalf("= %v, want %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected %q; a rename's destination is the file that exists", g)
		}
	}
}

func TestTheAgentRunsInTheClone(t *testing.T) {
	a := &fakeAgent{out: "done"}
	clone := pythonClone(t)
	if _, err := Run(context.Background(), a, &fakeJudge{}, &fakeGit{diff: "d"},
		&fakeRunner{}, &fakeRunner{}, candidate(), clone, nil, Options{}); err != nil {
		t.Fatal(err)
	}
	if a.clone != clone {
		t.Errorf("agent ran in %q, want %q", a.clone, clone)
	}
}

type fakeReverter struct {
	ran      [][]string
	applyErr error
}

func (f *fakeReverter) Git(_ context.Context, _ string, args ...string) (string, error) {
	f.ran = append(f.ran, args)
	if len(args) > 1 && args[0] == "stash" && args[1] == "pop" && f.applyErr != nil {
		return "", f.applyErr
	}
	return "", nil
}

// TestATestThatPassesEitherWayIsCaught is the point of the whole check. A patch
// can add a test that passes with or without the source change: it looks like
// verification, reads like verification in a pull request body, and proves
// nothing.
func TestATestThatPassesEitherWayIsCaught(t *testing.T) {
	g := &fakeGit{
		diff:    "diff --git a/x b/x\n+1",
		pending: "M\tinternal/sympath/walk.go\nM\tpkg/loader/load_test.go",
	}
	r := &fakeReverter{}
	// The test still passes with the source reverted.
	offline := &fakeRunner{replies: map[string]sandbox.Result{"go test": {Code: 0}}}

	got, err := ConfirmTestFailsFirst(context.Background(), g, r, offline,
		t.TempDir(), filepath.Join(t.TempDir(), "p.diff"), []string{"go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Checked {
		t.Fatal("the check did not run")
	}
	if got.FailedWithoutTheFix {
		t.Fatal("a test that passes without the fix was reported as exercising the bug")
	}
	if !strings.Contains(got.Why, "does not exercise the bug") {
		t.Errorf("why = %q", got.Why)
	}
}

func TestATestThatFailsFirstIsConfirmed(t *testing.T) {
	g := &fakeGit{
		diff:    "diff --git a/x b/x\n+1",
		pending: "M\tinternal/sympath/walk.go\nM\tpkg/loader/load_test.go",
	}
	r := &fakeReverter{}
	offline := &fakeRunner{replies: map[string]sandbox.Result{
		"go test": {Code: 1, Output: "--- FAIL: TestLoadDirWithBrokenSymlink"}}}

	got, err := ConfirmTestFailsFirst(context.Background(), g, r, offline,
		t.TempDir(), filepath.Join(t.TempDir(), "p.diff"), []string{"go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	if !got.FailedWithoutTheFix {
		t.Fatalf("got = %+v", got)
	}
	if !strings.Contains(got.Why, "fails without the fix") {
		t.Errorf("why = %q", got.Why)
	}
}

// TestOnlySourceIsReverted: reverting the test files too would leave nothing to
// run and the check would always "pass".
func TestOnlySourceIsReverted(t *testing.T) {
	g := &fakeGit{
		diff:    "d",
		pending: "M\tinternal/sympath/walk.go\nM\tpkg/loader/load_test.go\nM\tREADME.md",
	}
	r := &fakeReverter{}
	if _, err := ConfirmTestFailsFirst(context.Background(), g, r, &fakeRunner{},
		t.TempDir(), filepath.Join(t.TempDir(), "p.diff"), []string{"go test"}); err != nil {
		t.Fatal(err)
	}
	var reverted []string
	for _, args := range r.ran {
		if len(args) > 1 && args[0] == "stash" && args[1] == "push" {
			for i, a := range args {
				if a == "--" {
					reverted = args[i+1:]
				}
			}
		}
	}
	for _, p := range reverted {
		if strings.Contains(p, "_test.go") {
			t.Errorf("reverted a test file: %s", p)
		}
	}
	if len(reverted) != 2 {
		t.Errorf("reverted %v, want the two non-test files", reverted)
	}
}

// TestTheWorkIsAlwaysRestored, and a failure to restore is loud. Submitting a
// patch with its source half missing would be worse than any verification.
func TestTheWorkIsAlwaysRestored(t *testing.T) {
	dir := t.TempDir()
	patch := filepath.Join(dir, "p.diff")
	g := &fakeGit{diff: "diff --git a/x b/x\n+1",
		pending: "M\tsrc.go\nM\tsrc_test.go"}
	r := &fakeReverter{}

	if _, err := ConfirmTestFailsFirst(context.Background(), g, r, &fakeRunner{},
		dir, patch, []string{"go test"}); err != nil {
		t.Fatal(err)
	}
	var popped bool
	for _, args := range r.ran {
		if len(args) > 1 && args[0] == "stash" && args[1] == "pop" {
			popped = true
		}
	}
	if !popped {
		t.Fatal("the source change was never restored")
	}
	// And it is on disk before anything is touched, so a crash is recoverable.
	if b, err := os.ReadFile(patch); err != nil || !strings.Contains(string(b), "diff --git") {
		t.Fatalf("the patch was not saved: %v", err)
	}

	r2 := &fakeReverter{applyErr: errors.New("could not restore")}
	_, err := ConfirmTestFailsFirst(context.Background(), g, r2, &fakeRunner{},
		dir, patch, []string{"go test"})
	if err == nil {
		t.Fatal("a failed restore was swallowed")
	}
	for _, want := range []string{patch, "git stash list"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v; it must mention %q so the work can be found", err, want)
		}
	}
}

func TestNothingToHoldConstant(t *testing.T) {
	for _, tt := range []struct{ name, pending string }{
		{"tests only", "M\tsrc_test.go"},
		{"source only", "M\tsrc.go"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGit{diff: "d", pending: tt.pending}
			got, err := ConfirmTestFailsFirst(context.Background(), g, &fakeReverter{},
				&fakeRunner{}, t.TempDir(), filepath.Join(t.TempDir(), "p.diff"), []string{"go test"})
			if err != nil {
				t.Fatal(err)
			}
			if got.Checked {
				t.Error("claimed to have checked something it could not")
			}
			if got.Why == "" {
				t.Error("no reason given for not checking")
			}
		})
	}
}
