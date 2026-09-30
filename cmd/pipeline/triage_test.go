package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/store"
)

// fakeAPI answers the one GraphQL call contest and harvest both make.
type fakeAPI struct {
	thread string
	err    error
	calls  int
}

func (f *fakeAPI) GraphQL(_ context.Context, _ string, _ map[string]any, v any) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	return json.Unmarshal([]byte(f.thread), v)
}

// aThread is the smallest response harvest accepts: a maintainer stating an
// approach, which is what the brief is meant to find.
const aThread = `{"repository":{"nameWithOwner":"acme/widget","stargazerCount":900,
 "primaryLanguage":{"name":"Go"},"repositoryTopics":{"nodes":[]},
 "issue":{"number":7,"title":"broken thing","url":"https://github.com/acme/widget/issues/7",
  "body":"It breaks.","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-09-01T00:00:00Z",
  "author":{"login":"reporter"},"authorAssociation":"NONE",
  "reactions":{"totalCount":3},"labels":{"nodes":[{"name":"bug"}]},
  "comments":{"totalCount":1,"nodes":[
    {"author":{"login":"maint"},"authorAssociation":"MEMBER","createdAt":"2026-02-01T00:00:00Z",
     "body":"Guard the nil case in loadDir and add a test."}]},
  "timelineItems":{"nodes":[]}}}}`

type fakeBrief struct{ answer string }

func (f fakeBrief) JudgeWith(context.Context, string, string, string) (string, error) {
	if f.answer == "" {
		return `{"maintainer_desired_approach":"Guard the nil case in loadDir and add a test.",
		         "approach_source_url":"https://github.com/acme/widget/issues/7",
		         "approach_author_association":"MEMBER",
		         "acceptance_criteria":["a test covering the nil case"]}`, nil
	}
	return f.answer, nil
}

type memStore struct {
	queue []*model.Candidate
	saves int
	facts *model.RepoFacts
}

func (m *memStore) ByStatus(...model.Status) []*model.Candidate { return m.queue }
func (m *memStore) Save(*model.Candidate) (string, error)       { m.saves++; return "", nil }
func (m *memStore) LoadRepoFacts(string) (*model.RepoFacts, error) {
	if m.facts == nil {
		return nil, errors.New("no facts")
	}
	return m.facts, nil
}

func goodFacts() *model.RepoFacts {
	return &model.RepoFacts{
		Repo: "acme/widget", PrimaryLanguage: "Go", Stars: 900,
		HasTests: true, HasContributing: true, MergedFirstTimePR90d: true,
		FetchedAt: "2026-09-30T00:00:00Z",
	}
}

func discovered(n int) []*model.Candidate {
	var out []*model.Candidate
	for i := 0; i < n; i++ {
		out = append(out, &model.Candidate{
			Repo: "acme/widget", Issue: 7 + i, Title: "broken thing",
			URL: "https://github.com/acme/widget/issues/7", Status: model.StatusDiscovered,
			Labels: []string{"good first issue"}, Reactions: 3,
			IssueCreatedAt: "2026-01-01T00:00:00Z", IssueUpdatedAt: "2026-09-01T00:00:00Z",
		})
	}
	return out
}

func triageFixture(t *testing.T, n int) (triage, *memStore, *fakeAPI) {
	t.Helper()
	cfg, err := policy.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	st := &memStore{queue: discovered(n), facts: goodFacts()}
	api := &fakeAPI{thread: aThread}
	return triage{
		root: t.TempDir(), cfg: cfg, gh: api, brain: fakeBrief{}, st: st,
		log:     &stubLog{},
		facts:   func(context.Context, string) (*model.RepoFacts, error) { return goodFacts(), nil },
		execute: true, maxHarvest: 12,
	}, st, api
}

func TestTriageDryRunWritesNothing(t *testing.T) {
	tr, st, _ := triageFixture(t, 3)
	tr.execute = false
	if code := tr.run(context.Background(), st.queue); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if st.saves != 0 {
		t.Fatalf("a dry run saved %d candidate(s)", st.saves)
	}
	for _, c := range st.queue {
		if c.Status != model.StatusDiscovered {
			t.Fatalf("%s moved to %s in a dry run", c.Slug(), c.Status)
		}
	}
}

func TestTriageStopsAtProposed(t *testing.T) {
	// The human gate is structural. Nothing in intake may reach approved, and
	// the transition table is only half the guarantee -- the other half is
	// that no code here tries.
	tr, st, _ := triageFixture(t, 1)
	if code := tr.run(context.Background(), st.queue); code != 0 {
		t.Fatalf("exit %d", code)
	}
	c := st.queue[0]
	if c.Status != model.StatusProposed {
		t.Fatalf("status = %s, want proposed", c.Status)
	}
	for _, h := range c.History {
		if h.To == string(model.StatusApproved) || h.To == string(model.StatusAutoApproved) {
			t.Fatalf("intake wrote an approval: %+v", h)
		}
	}
	if c.Brief == nil || c.Brief.MaintainerDesiredApproach == "" {
		t.Fatal("proposed without a maintainer approach")
	}
}

func TestTriageDefersOverTheHarvestCapRatherThanDroppingWork(t *testing.T) {
	// The overflow must be rejected with a reason ShouldReconsider treats as
	// transient, or a candidate that merely arrived late is gone for good.
	tr, st, _ := triageFixture(t, 5)
	tr.maxHarvest = 2
	if code := tr.run(context.Background(), st.queue); code != 0 {
		t.Fatalf("exit %d", code)
	}
	proposed, deferred := 0, 0
	for _, c := range st.queue {
		switch {
		case c.Status == model.StatusProposed:
			proposed++
		case strings.Contains(c.RejectReason, "deferred: exceeded max_harvest"):
			deferred++
			if !store.IsTransient(c.RejectReason) {
				t.Fatalf("a deferral that never comes back: %q", c.RejectReason)
			}
		}
	}
	if proposed != 2 || deferred != 3 {
		t.Fatalf("proposed=%d deferred=%d, want 2 and 3", proposed, deferred)
	}
}

func TestTriageRejectsAContestedIssueWithoutHarvestingIt(t *testing.T) {
	// Phase A is first because everything after it is wasted on an issue
	// somebody else is already working on.
	tr, st, api := triageFixture(t, 1)
	st.queue[0].LinkedPRs = []int{99}
	api.thread = `{"repository":{"pullRequest":{"number":99,"state":"OPEN","isDraft":false,
	  "createdAt":"2026-09-20T00:00:00Z","updatedAt":"2026-09-29T00:00:00Z",
	  "author":{"login":"someone"},"labels":{"nodes":[]},"commits":{"nodes":[]},
	  "reviews":{"nodes":[]},"comments":{"totalCount":0,"nodes":[]}}}}`
	if code := tr.run(context.Background(), st.queue); code != 0 {
		t.Fatalf("exit %d", code)
	}
	c := st.queue[0]
	if c.Status != model.StatusRejected || !strings.HasPrefix(c.RejectReason, "contest=") {
		t.Fatalf("status=%s reason=%q", c.Status, c.RejectReason)
	}
	if c.Brief != nil {
		t.Fatal("harvested an issue that was already rejected")
	}
}

func TestTriageSurvivesAHarvestFailureWithoutLosingTheRest(t *testing.T) {
	tr, st, api := triageFixture(t, 2)
	api.err = fmt.Errorf("upstream 502")
	if code := tr.run(context.Background(), st.queue); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, c := range st.queue {
		if c.Status != model.StatusRejected || !strings.Contains(c.RejectReason, "harvest failed") {
			t.Fatalf("%s: status=%s reason=%q", c.Slug(), c.Status, c.RejectReason)
		}
	}
}

func TestWithoutThreadBarsKeepsOnlyWhatPhaseBCanAnswer(t *testing.T) {
	// Phase B runs before the thread exists. Rejecting on a bar only a brief
	// can answer would reject every candidate for not having been harvested.
	in := []string{
		"acme/widget is manually excluded",
		"no brief (harvest did not run)",
		"no maintainer acceptance (no triage-gated label)",
		"thread has not converged",
		"claimed by someone 3 days ago",
		"repository has no tests",
	}
	got := withoutThreadBars(in)
	want := []string{"acme/widget is manually excluded", "repository has no tests"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestLinkedPRsRebuildTheURL(t *testing.T) {
	c := &model.Candidate{Repo: "helm/helm", LinkedPRs: []int{32116, 32705}}
	got := linkedPRs(c)
	if len(got) != 2 || got[0].URL != "https://github.com/helm/helm/pull/32116" {
		t.Fatalf("got %+v", got)
	}
}
