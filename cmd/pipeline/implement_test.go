package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vimalyad/osspipeline/internal/ghx"
	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/notify"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/repo"
	"github.com/vimalyad/osspipeline/internal/store"
	"github.com/vimalyad/osspipeline/internal/submit"
)

// The publishing sequence is the only code in this project that can put
// something in front of a maintainer, and it is also the code with the least
// margin: a status written a step too early or too late cannot be corrected
// once a branch is public. These tests pin the sequence, one failure at a time.

type stubGit struct {
	steps      []string
	forkErr    error
	identErr   error
	commitErr  error
	pushErr    error
	commitMsgs []string
}

func (g *stubGit) Git(_ context.Context, _ string, args ...string) (string, error) {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	switch verb {
	case "commit":
		g.steps = append(g.steps, "commit")
		if len(args) > 2 {
			g.commitMsgs = append(g.commitMsgs, args[2])
		}
		return "", g.commitErr
	case "push":
		g.steps = append(g.steps, "push")
		return "", g.pushErr
	case "diff":
		return "diff --git a/x b/x\n+one line\n", nil
	case "rev-list":
		return "0", nil
	}
	return "", nil
}
func (g *stubGit) DefaultBranch(context.Context, string) string { return "main" }
func (g *stubGit) TopLevelEntries(context.Context, string) map[string]bool {
	return map[string]bool{"README.md": true}
}
func (g *stubGit) CommitStyle(context.Context, string) string { return "fix: subject" }

func (g *stubGit) EnsureFork(_ context.Context, _ repo.Forker, r, _ string) (string, error) {
	g.steps = append(g.steps, "fork")
	if g.forkErr != nil {
		return "", g.forkErr
	}
	_, name, _ := strings.Cut(r, "/")
	return "vimalyad/" + name, nil
}

func (g *stubGit) AssertIdentity(string) error {
	g.steps = append(g.steps, "assert-identity")
	return g.identErr
}

type stubGH struct {
	steps []string
	url   string
	err   error
}

func (h *stubGH) RESTRaw(_ context.Context, args []string, _ string) (string, error) {
	h.steps = append(h.steps, strings.Join(args, " "))
	if h.err != nil {
		return "", h.err
	}
	return h.url, nil
}

func (h *stubGH) opened() bool {
	for _, s := range h.steps {
		if strings.Contains(s, "pr create") {
			return true
		}
	}
	return false
}

type stubStore struct {
	all     []*model.Candidate
	saved   []model.Status
	saveErr error
}

func (s *stubStore) All() ([]*model.Candidate, []store.LoadResult) { return s.all, nil }
func (s *stubStore) Save(c *model.Candidate) (string, error) {
	s.saved = append(s.saved, c.Status)
	return "", s.saveErr
}

type stubJudge struct{ out string }

func (j stubJudge) JudgeWith(context.Context, string, string, string) (string, error) {
	return j.out, nil
}

type stubLog struct{ kinds []string }

func (l *stubLog) Record(kind, _, _ string) error {
	l.kinds = append(l.kinds, kind)
	return nil
}

func (l *stubLog) recorded(kind string) bool {
	for _, k := range l.kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func fixture(t *testing.T) (publisher, *stubGit, *stubGH, *stubStore, *stubLog, *model.Candidate) {
	t.Helper()
	c := &model.Candidate{Repo: "helm/helm", Issue: 13284, Status: model.StatusApproved,
		Title: "broken symlinks", Branch: "fix/issue-13284"}
	g := &stubGit{}
	h := &stubGH{url: "https://github.com/helm/helm/pull/9001"}
	st := &stubStore{all: []*model.Candidate{c}}
	lg := &stubLog{}
	p := publisher{
		root: "../..",
		caps: policy.Caps{PRsPerDay: 2, MaxOpenPRs: 5, MaxOpenPerRepo: 1, OrgCooldownDays: 3},
		st:   st, log: lg, git: g, gh: h,
		brain: stubJudge{out: "fix: handle a broken symlink"},
		id:    submit.Identity{Name: "V", Email: "v@example.com", Login: "vimalyad"},
	}
	return p, g, h, st, lg, c
}

func plan() submit.Plan {
	return submit.Plan{Title: "fix: handle a broken symlink",
		Body:   "Symlinks that point nowhere aborted chart loading.\n\nFixes #13284\n",
		Branch: "fix/issue-13284", Base: "main", Head: "vimalyad:fix/issue-13284"}
}

func TestSubmitHappyPathOrdersEveryStep(t *testing.T) {
	p, g, h, st, lg, c := fixture(t)
	if code := p.submit(context.Background(), c, "/clone", plan(), "go"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	// Fork first, identity re-checked once the push destination exists, then
	// commit, then push, then open.
	want := "fork assert-identity commit push"
	if got := strings.Join(g.steps, " "); got != want {
		t.Fatalf("git steps = %q, want %q", got, want)
	}
	if !h.opened() {
		t.Fatal("no pull request was opened")
	}
	wantStatuses := []model.Status{
		model.StatusImplementing, model.StatusImplemented, model.StatusPushed,
		model.StatusPushed, // the number, recorded before the status
		model.StatusPROpen,
	}
	if fmt.Sprint(st.saved) != fmt.Sprint(wantStatuses) {
		t.Fatalf("saved %v, want %v", st.saved, wantStatuses)
	}
	if c.Status != model.StatusPROpen || c.PRNumber == nil || *c.PRNumber != 9001 {
		t.Fatalf("status=%s number=%v", c.Status, c.PRNumber)
	}
	for _, kind := range []string{"fork", "push", "pr_open"} {
		if !lg.recorded(kind) {
			t.Errorf("nothing audited for %q", kind)
		}
	}
}

func TestSubmitStopsAtACapWithoutTouchingAnything(t *testing.T) {
	p, g, h, st, _, c := fixture(t)
	// One pull request already open on this repository.
	st.all = append(st.all, &model.Candidate{Repo: "helm/helm", Issue: 1,
		Status: model.StatusPROpen})
	if code := p.submit(context.Background(), c, "/clone", plan(), "go"); code == 0 {
		t.Fatal("submitted over a cap")
	}
	if len(g.steps) > 0 || len(h.steps) > 0 {
		t.Fatalf("a cap breach still did work: git=%v gh=%v", g.steps, h.steps)
	}
	// Still approved, so the next run retries it. Nothing to recover.
	if c.Status != model.StatusApproved {
		t.Fatalf("status = %s, want it left approved", c.Status)
	}
	if len(st.saved) != 0 {
		t.Fatalf("wrote state for a run that did nothing: %v", st.saved)
	}
}

func TestSubmitAbandonsWhenTheForkCannotBeMade(t *testing.T) {
	p, g, h, _, _, c := fixture(t)
	g.forkErr = fmt.Errorf("%w: rate limited", repo.ErrFork)
	if code := p.submit(context.Background(), c, "/clone", plan(), "go"); code == 0 {
		t.Fatal("submitted without a fork")
	}
	if got := strings.Join(g.steps, " "); got != "fork" {
		t.Fatalf("went past the fork: %q", got)
	}
	if h.opened() {
		t.Fatal("opened a pull request with no fork")
	}
	if c.Status != model.StatusAbandoned {
		t.Fatalf("status = %s, want abandoned so retry can recover it", c.Status)
	}
}

func TestSubmitStopsIfTheCloneIsNoLongerTheOSSAccount(t *testing.T) {
	// The fork remote exists by this point, so this is the first moment the
	// push destination can be checked -- and the last before a branch leaves
	// the machine under a name that must not be linkable to the user.
	p, g, h, _, _, c := fixture(t)
	g.identErr = errors.New(`remote "fork" pushes to another account`)
	if code := p.submit(context.Background(), c, "/clone", plan(), "go"); code == 0 {
		t.Fatal("pushed from a clone that failed its identity check")
	}
	if got := strings.Join(g.steps, " "); got != "fork assert-identity" {
		t.Fatalf("went past the identity check: %q", got)
	}
	if h.opened() {
		t.Fatal("opened a pull request anyway")
	}
	if c.Status != model.StatusAbandoned {
		t.Fatalf("status = %s", c.Status)
	}
}

func TestSubmitRefusesAnAttributedCommitMessage(t *testing.T) {
	// The one rule that cannot be undone after a maintainer has pulled the
	// commit, enforced here rather than trusted to the prompt.
	p, g, h, _, _, c := fixture(t)
	p.brain = stubJudge{out: "fix: x\n\nCo-Authored-By: Claude <noreply@anthropic.com>"}
	if code := p.submit(context.Background(), c, "/clone", plan(), "go"); code == 0 {
		t.Fatal("committed an attributed message")
	}
	if strings.Contains(strings.Join(g.steps, " "), "commit") {
		t.Fatalf("reached the commit: %v", g.steps)
	}
	if h.opened() {
		t.Fatal("opened a pull request anyway")
	}
	if c.Status != model.StatusAbandoned {
		t.Fatalf("status = %s", c.Status)
	}
}

func TestSubmitDoesNotRecordPushedWhenThePushFailed(t *testing.T) {
	// The status the watcher acts on. It fetches fork/<branch> and refuses to
	// touch a clone that is ahead of it, so "pushed" around a failed push is
	// not a cosmetic inaccuracy.
	p, g, h, st, lg, c := fixture(t)
	g.pushErr = errors.New("rejected: non-fast-forward")
	if code := p.submit(context.Background(), c, "/clone", plan(), "go"); code == 0 {
		t.Fatal("reported success after a failed push")
	}
	if h.opened() {
		t.Fatal("opened a pull request for a branch that was never pushed")
	}
	for _, s := range st.saved {
		if s == model.StatusPushed {
			t.Fatalf("recorded pushed: %v", st.saved)
		}
	}
	if lg.recorded("push") {
		t.Fatal("audited a push that did not happen")
	}
	if c.Status != model.StatusAbandoned {
		t.Fatalf("status = %s, want abandoned so retry can re-run it", c.Status)
	}
}

func TestSubmitLeavesAPushedBranchRecordedWhenOpeningFails(t *testing.T) {
	// The opposite error to the one above, and it has the opposite answer: the
	// branch really is public, so the status has to say so even though the
	// pipeline cannot finish on its own.
	p, g, h, _, _, c := fixture(t)
	h.err = fmt.Errorf("%w: gh pr create", ghx.ErrGh)
	if code := p.submit(context.Background(), c, "/clone", plan(), "go"); code == 0 {
		t.Fatal("reported success without a pull request")
	}
	if !strings.Contains(strings.Join(g.steps, " "), "push") {
		t.Fatalf("never pushed: %v", g.steps)
	}
	if c.Status != model.StatusPushed {
		t.Fatalf("status = %s, want pushed -- the branch is on the fork", c.Status)
	}
}

func TestSubmitCapsAreReadAtTheMomentOfPublishing(t *testing.T) {
	// The cap is checked again here because the container work in between
	// takes long enough for the watcher to have opened something.
	p, _, _, st, _, c := fixture(t)
	st.all = append(st.all, &model.Candidate{Repo: "other/repo", Issue: 2,
		Status: model.StatusMerged, History: []model.HistoryEntry{
			{At: time.Now().UTC().Format(time.RFC3339), To: string(model.StatusPROpen)},
			{At: time.Now().UTC().Format(time.RFC3339), To: string(model.StatusPROpen)},
		}},
		&model.Candidate{Repo: "third/repo", Issue: 3, Status: model.StatusMerged,
			History: []model.HistoryEntry{
				{At: time.Now().UTC().Format(time.RFC3339), To: string(model.StatusPROpen)},
			}})
	if code := p.submit(context.Background(), c, "/clone", plan(), "go"); code == 0 {
		t.Fatal("submitted past the daily cap")
	}
	if c.Status != model.StatusApproved {
		t.Fatalf("status = %s", c.Status)
	}
}

func TestSubmitWillNotPushForABodyItWouldRefuseToSend(t *testing.T) {
	// Open re-checks the body as a last backstop, but by then the branch is
	// already on the fork. A refused body must not cost a public branch that
	// no pull request will ever explain.
	p, g, h, st, _, c := fixture(t)
	pl := plan()
	pl.Body = "I don't see any diff content in your message.\n\nFixes #13284\n"
	if code := p.submit(context.Background(), c, "/clone", pl, "go"); code == 0 {
		t.Fatal("submitted a body the guard rejects")
	}
	if len(g.steps) > 0 || h.opened() {
		t.Fatalf("did work before checking the body: git=%v gh=%v", g.steps, h.steps)
	}
	if c.Status != model.StatusApproved || len(st.saved) != 0 {
		t.Fatalf("status=%s saved=%v -- nothing happened, so nothing should be recorded",
			c.Status, st.saved)
	}
}

func TestSubmitNotifiesOnlyAfterThePullRequestIsOpen(t *testing.T) {
	p, _, h, _, _, c := fixture(t)
	var events []notify.Event
	p.notify = func(e notify.Event) { events = append(events, e) }

	if code := p.submit(context.Background(), c, "/clone", plan(), "go"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(events) != 1 || events[0].Kind != notify.KindPROpened {
		t.Fatalf("events = %+v", events)
	}
	if events[0].URL != "https://github.com/helm/helm/pull/9001" {
		t.Fatalf("the notification does not link to the pull request: %q", events[0].URL)
	}

	// And nothing when it fails: a push that never became a pull request must
	// not tell the user one is open.
	p2, _, h2, _, _, c2 := fixture(t)
	events = nil
	p2.notify = func(e notify.Event) { events = append(events, e) }
	h2.err = fmt.Errorf("%w: gh pr create", ghx.ErrGh)
	if code := p2.submit(context.Background(), c2, "/clone", plan(), "go"); code == 0 {
		t.Fatal("reported success")
	}
	if len(events) != 0 {
		t.Fatalf("notified about a pull request that does not exist: %+v", events)
	}
	_ = h
}

// There must be no way to post every queued reply at once. A reply goes out
// under the user's name to a person waiting for it, and the design asks one
// question at a time with the full text in front of them. A sweep would answer
// that question on their behalf, which is exactly what left five drafts unsent
// for days in v1 -- the opposite failure, same cause: nobody decided.
func TestNoBulkReplyPosting(t *testing.T) {
	src, err := os.ReadFile("replies.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "replies.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	// replies.Post may be called from exactly one function, and not from a
	// loop inside it.
	callers := map[string]int{}
	var inLoop []string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		ast.Inspect(fn, func(m ast.Node) bool {
			switch loop := m.(type) {
			case *ast.RangeStmt, *ast.ForStmt:
				ast.Inspect(loop.(ast.Node), func(k ast.Node) bool {
					if isRepliesPost(k) {
						inLoop = append(inLoop, fn.Name.Name)
					}
					return true
				})
			}
			if isRepliesPost(m) {
				callers[fn.Name.Name]++
			}
			return true
		})
		return true
	})
	if len(inLoop) > 0 {
		t.Fatalf("replies.Post is called inside a loop in %v", inLoop)
	}
	if len(callers) != 1 || callers["repliesPost"] != 1 {
		t.Fatalf("replies.Post callers = %v, want exactly repliesPost once", callers)
	}
}

func isRepliesPost(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Post" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "replies"
}
