package gate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/store"
	"gopkg.in/yaml.v3"
)

type fakeStore struct {
	cands map[string]*model.Candidate
	saves int
}

func (f *fakeStore) Load(slug string) (*model.Candidate, error) {
	c, ok := f.cands[slug]
	if !ok {
		return nil, errors.New("no such candidate")
	}
	return c, nil
}
func (f *fakeStore) Save(c *model.Candidate) (string, error) { f.saves++; return "", nil }
func (f *fakeStore) All() ([]*model.Candidate, []store.LoadResult) {
	var out []*model.Candidate
	for _, k := range sortedKeys(f.cands) {
		out = append(out, f.cands[k])
	}
	return out, nil
}

func sortedKeys(m map[string]*model.Candidate) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j] < ks[j-1]; j-- {
			ks[j], ks[j-1] = ks[j-1], ks[j]
		}
	}
	return ks
}

type fakeAudit struct{ entries []string }

func (f *fakeAudit) Record(kind, slug, detail string) error {
	f.entries = append(f.entries, kind+"|"+slug+"|"+detail)
	return nil
}

func cand(repo string, issue int, st model.Status) *model.Candidate {
	return &model.Candidate{Repo: repo, Issue: issue, Status: st, URL: "https://x"}
}

func storeWith(cs ...*model.Candidate) *fakeStore {
	f := &fakeStore{cands: map[string]*model.Candidate{}}
	for _, c := range cs {
		f.cands[c.Slug()] = c
	}
	return f
}

func TestApprove(t *testing.T) {
	c := cand("helm/helm", 13284, model.StatusProposed)
	s, a := storeWith(c), &fakeAudit{}

	msg, err := Approve(s, a, c.Slug())
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != model.StatusApproved {
		t.Fatalf("status = %s", c.Status)
	}
	if !strings.Contains(msg, "helm/helm#13284") {
		t.Errorf("msg = %q; a phone notification needs the repo and issue", msg)
	}
	if len(a.entries) != 1 || !strings.HasPrefix(a.entries[0], "approve|") {
		t.Errorf("audit = %v", a.entries)
	}
}

// TestApprovingTwiceSaysWhichItWas: a second tap on a notification is the
// common case, and silently doing nothing leaves the user unsure whether the
// first tap registered.
func TestApprovingTwiceSaysWhichItWas(t *testing.T) {
	c := cand("a/b", 1, model.StatusApproved)
	s := storeWith(c)
	_, err := Approve(s, nil, c.Slug())
	if !errors.Is(err, ErrNotProposed) {
		t.Fatalf("err = %v, want ErrNotProposed", err)
	}
	if !strings.Contains(err.Error(), "approved") {
		t.Errorf("err = %v; it must say what the status actually is", err)
	}
	if s.saves != 0 {
		t.Error("a no-op approval wrote to disk")
	}
}

// TestRejectionNeedsAReason: a rejection with no reason cannot be told apart
// from one the pipeline made itself, and the two are treated differently --
// a human rejection is never reconsidered.
func TestRejectionNeedsAReason(t *testing.T) {
	c := cand("a/b", 1, model.StatusProposed)
	s := storeWith(c)
	for _, reason := range []string{"", "   ", "\t\n"} {
		if _, err := Reject(s, nil, c.Slug(), reason); err == nil {
			t.Fatalf("accepted an empty reason %q", reason)
		}
	}
	if c.Status != model.StatusProposed {
		t.Error("status changed despite the refusal")
	}
}

func TestRejectRecordsTheReasonAsHuman(t *testing.T) {
	c := cand("a/b", 1, model.StatusProposed)
	s, a := storeWith(c), &fakeAudit{}
	if _, err := Reject(s, a, c.Slug(), "not worth the review time"); err != nil {
		t.Fatal(err)
	}
	if c.Status != model.StatusRejected {
		t.Fatalf("status = %s", c.Status)
	}
	if !strings.HasPrefix(c.RejectReason, "human rejection:") {
		t.Errorf("reject_reason = %q; the reconsider logic keys off this prefix", c.RejectReason)
	}
	if len(c.History) == 0 || !strings.Contains(c.History[len(c.History)-1].Note, "not worth") {
		t.Error("the reason is not in the history")
	}
}

func TestRejectingATerminalCandidateIsANoOp(t *testing.T) {
	c := cand("a/b", 1, model.StatusMerged)
	s := storeWith(c)
	// Merged is terminal; the table forbids the edge, and we must not claim
	// to have rejected something that shipped.
	if _, err := Reject(s, nil, c.Slug(), "changed my mind"); !errors.Is(err, model.ErrIllegalTransition) {
		t.Fatalf("err = %v, want ErrIllegalTransition", err)
	}
	if c.Status != model.StatusMerged {
		t.Fatalf("status = %s", c.Status)
	}
}

func exclusionsIn(t *testing.T, body string) Exclusions {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "exclusions.yaml")
	if body != "" {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Exclusions{Path: p}
}

// TestExcludeDropsWhatIsAlreadyQueued: adding a name to a list while four
// candidates sit approved would let them through on the next cycle, which is
// the opposite of what the command is for.
func TestExcludeDropsWhatIsAlreadyQueued(t *testing.T) {
	queued := []*model.Candidate{
		cand("a/b", 1, model.StatusDiscovered),
		cand("a/b", 2, model.StatusProposed),
		cand("a/b", 3, model.StatusApproved),
		cand("a/b", 4, model.StatusMerged),   // terminal: must be left alone
		cand("c/d", 5, model.StatusProposed), // another repo: untouched
	}
	s, a := storeWith(queued...), &fakeAudit{}
	ex := exclusionsIn(t, "exclude_repos:\n  - old/repo\n")

	msg, err := Exclude(s, a, ex, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "dropped 3") {
		t.Errorf("msg = %q, want 3 dropped", msg)
	}
	if queued[3].Status != model.StatusMerged {
		t.Error("a merged candidate was rejected")
	}
	if queued[4].Status != model.StatusProposed {
		t.Error("a candidate in another repository was dropped")
	}
	for _, c := range queued[:3] {
		if c.Status != model.StatusRejected {
			t.Errorf("%s is %s, want rejected", c.Slug(), c.Status)
		}
	}
}

// TestExcludePreservesEveryOtherKey: the file also holds banned_ai_policy and
// cla_signed, and a writer that only knew about its own key would drop them.
func TestExcludePreservesEveryOtherKey(t *testing.T) {
	ex := exclusionsIn(t, `exclude_repos:
  - old/repo
banned_ai_policy:
  - some/repo
cla_signed:
  - signed/repo
`)
	if _, err := Exclude(storeWith(), nil, ex, "new/repo"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ex.Path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string][]string
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc["banned_ai_policy"]) != 1 || len(doc["cla_signed"]) != 1 {
		t.Fatalf("other keys were lost: %+v", doc)
	}
	want := []string{"new/repo", "old/repo"}
	if len(doc["exclude_repos"]) != 2 || doc["exclude_repos"][0] != want[0] {
		t.Fatalf("exclude_repos = %v, want %v sorted", doc["exclude_repos"], want)
	}
}

func TestExcludeIsIdempotent(t *testing.T) {
	ex := exclusionsIn(t, "exclude_repos:\n  - a/b\n")
	s := storeWith(cand("a/b", 1, model.StatusProposed))
	msg, err := Exclude(s, nil, ex, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "already excluded") {
		t.Errorf("msg = %q", msg)
	}
	// And it must not have dropped anything on the second run either: the
	// first run already did, and re-reporting would be misleading.
	if s.saves != 0 {
		t.Error("a repeated exclusion wrote candidates again")
	}
}

func TestExcludeCreatesTheFileWhenMissing(t *testing.T) {
	ex := exclusionsIn(t, "")
	if _, err := Exclude(storeWith(), nil, ex, "a/b"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ex.Path)
	if err != nil {
		t.Fatalf("the file was not created: %v", err)
	}
	if !strings.Contains(string(b), "a/b") {
		t.Errorf("contents = %q", b)
	}
}

func TestCLASignedClearsOnlyTheCLABlocker(t *testing.T) {
	c := cand("a/b", 1, model.StatusProposed)
	c.Blockers = []string{
		"CLA required for a -- sign once, then `pipeline cla-signed a/b`",
		"Go toolchain missing -- install go",
	}
	other := cand("c/d", 2, model.StatusProposed)
	other.Blockers = []string{"CLA required for c"}

	s, a := storeWith(c, other), &fakeAudit{}
	ex := exclusionsIn(t, "")
	msg, err := CLASigned(s, a, ex, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Blockers) != 1 || !strings.Contains(c.Blockers[0], "toolchain") {
		t.Fatalf("blockers = %v; only the CLA one should go", c.Blockers)
	}
	if len(other.Blockers) != 1 {
		t.Error("another repository's blocker was cleared")
	}
	if !strings.Contains(msg, "unblocked 1") {
		t.Errorf("msg = %q", msg)
	}
	if len(a.entries) != 1 || !strings.HasPrefix(a.entries[0], "cla_signed|") {
		t.Errorf("audit = %v", a.entries)
	}
}

func TestLoadFailurePropagates(t *testing.T) {
	if _, err := Approve(storeWith(), nil, "nope__nope__1"); !errors.Is(err, ErrGate) {
		t.Fatalf("err = %v", err)
	}
}

func TestRejectUpgradesAMachineRejectionToAHumanOne(t *testing.T) {
	// The scorer parks a candidate with a reason that expires; a person
	// decides. Without this the decision is made again every fortnight.
	c := cand("a/b", 1, model.StatusRejected)
	c.RejectReason = "deferred: exceeded max_harvest=12 this run"
	before := len(c.History)
	s, a := storeWith(c), &fakeAudit{}

	msg, err := Reject(s, a, c.Slug(), "the maintainers ruled out both fixes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.RejectReason, "human rejection:") {
		t.Fatalf("reject_reason = %q", c.RejectReason)
	}
	// The status did not move, so nothing may be written to the history: a
	// rejected -> rejected entry is not an edge the table has, and `doctor`
	// checks every edge strictly.
	if len(c.History) != before {
		t.Fatalf("wrote %d history entries for a status that did not change",
			len(c.History)-before)
	}
	if len(a.entries) == 0 {
		t.Error("the decision was not audited")
	}
	if !strings.Contains(msg, "stays rejected") {
		t.Errorf("unclear message: %q", msg)
	}
}

func TestRejectDoesNotRewriteAnEarlierHumanRejection(t *testing.T) {
	c := cand("a/b", 1, model.StatusRejected)
	c.RejectReason = "human rejection: the first reason"
	s := storeWith(c)
	if _, err := Reject(s, &fakeAudit{}, c.Slug(), "a second reason"); err != nil {
		t.Fatal(err)
	}
	if c.RejectReason != "human rejection: the first reason" {
		t.Fatalf("overwrote the original decision: %q", c.RejectReason)
	}
}

func TestRejectingAnAbandonedCandidateIsANoOp(t *testing.T) {
	c := cand("a/b", 1, model.StatusAbandoned)
	c.RejectReason = "push failed"
	s := storeWith(c)
	if _, err := Reject(s, &fakeAudit{}, c.Slug(), "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if c.RejectReason != "push failed" || c.Status != model.StatusAbandoned {
		t.Fatalf("status=%s reason=%q", c.Status, c.RejectReason)
	}
}
