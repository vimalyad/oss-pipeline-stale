package propose

import (
	"strings"
	"testing"
	"time"

	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/profile"
)

var day = time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)

type fakeDomains struct{ byRepo map[string]profile.Domain }

func (f *fakeDomains) ForRepo(repo, _ string, _ []string) (profile.Domain, bool) {
	d, ok := f.byRepo[repo]
	return d, ok
}

func cand(repo string, issue int, st model.Status) *model.Candidate {
	return &model.Candidate{
		Repo: repo, Issue: issue, Status: st, Title: "a title",
		URL: "https://x", Contest: model.ContestNoPR,
	}
}

var domains = &fakeDomains{byRepo: map[string]profile.Domain{
	"kornia/kornia": {ID: "python-cv", Label: "Computer vision", Weight: 3},
	"helm/helm":     {ID: "devops-go", Label: "Kubernetes tooling", Weight: 2},
	"some/cli":      {ID: "cli-tools", Label: "CLI tools", Weight: 1},
}}

var caps = policy.Caps{PRsPerDay: 2, MaxOpenPRs: 5, MaxOpenPerRepo: 1}

// TestWeightDrivesTheOrder is what makes the profile a steering wheel: raising
// one number moves a whole area up the page with no code change.
func TestWeightDrivesTheOrder(t *testing.T) {
	cands := []*model.Candidate{
		cand("some/cli", 1, model.StatusProposed),
		cand("helm/helm", 2, model.StatusProposed),
		cand("kornia/kornia", 3, model.StatusProposed),
		cand("unknown/repo", 4, model.StatusProposed),
	}
	got, _, _ := Classify(cands, domains)
	var order []string
	for _, e := range got {
		order = append(order, e.Candidate.Repo)
	}
	want := []string{"kornia/kornia", "helm/helm", "some/cli", "unknown/repo"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestBlockedWorkSinksBelowEverything: a blocker needs a human action outside
// the pipeline, so nothing can be approved until it clears -- however heavy its
// domain.
func TestBlockedWorkSinksBelowEverything(t *testing.T) {
	blocked := cand("kornia/kornia", 1, model.StatusProposed)
	blocked.Blockers = []string{"CLA required"}
	clear := cand("some/cli", 2, model.StatusProposed)

	got, _, _ := Classify([]*model.Candidate{blocked, clear}, domains)
	if got[0].Candidate.Repo != "some/cli" {
		t.Fatalf("blocked weight-3 work outranked clear weight-1 work: %s",
			got[0].Candidate.Repo)
	}
}

func TestTieBreaksAreDeterministic(t *testing.T) {
	// Same domain, same penalties, same reactions: only the slug separates
	// them, and two runs over unchanged state must produce the same page.
	var cands []*model.Candidate
	for _, n := range []int{9, 3, 7, 1} {
		cands = append(cands, cand("kornia/kornia", n, model.StatusProposed))
	}
	var first string
	for run := 0; run < 5; run++ {
		got, _, _ := Classify(cands, domains)
		var order []string
		for _, e := range got {
			order = append(order, e.Candidate.Slug())
		}
		joined := strings.Join(order, ",")
		if run == 0 {
			first = joined
			continue
		}
		if joined != first {
			t.Fatalf("run %d = %s, run 0 = %s", run, joined, first)
		}
	}
	if !strings.HasPrefix(first, "kornia__kornia__1,") {
		t.Errorf("order = %s, want slug order", first)
	}
}

func TestSoftPenaltiesAndReactionsRankWithinADomain(t *testing.T) {
	clean := cand("kornia/kornia", 1, model.StatusProposed)
	clean.Reactions = 2
	penalised := cand("kornia/kornia", 2, model.StatusProposed)
	penalised.SoftPenalties = []string{"issue is 5 years old"}
	penalised.Reactions = 50
	popular := cand("kornia/kornia", 3, model.StatusProposed)
	popular.Reactions = 40

	got, _, _ := Classify([]*model.Candidate{penalised, clean, popular}, domains)
	want := []int{3, 1, 2} // popular, clean, then the penalised one last
	for i, w := range want {
		if got[i].Candidate.Issue != w {
			t.Fatalf("order = %d,%d,%d, want %v",
				got[0].Candidate.Issue, got[1].Candidate.Issue, got[2].Candidate.Issue, want)
		}
	}
}

// TestCommentOpportunitiesStaySeparate: someone else's live work is not a
// target, and blurring the two sections is how a competing PR gets opened.
func TestCommentOpportunitiesStaySeparate(t *testing.T) {
	live := cand("kornia/kornia", 1, model.StatusRejected)
	live.Contest = model.ContestActivePR
	live.PRSignal = &model.PRSignal{Number: 99, URL: "https://pr", Author: "someone",
		Reasons: []string{"author active 2d ago"}}
	plain := cand("helm/helm", 2, model.StatusRejected)
	plain.RejectReason = "requires a CLA"

	proposed, rejected, ops := Classify([]*model.Candidate{live, plain}, domains)
	if len(proposed) != 0 {
		t.Fatal("a rejected candidate reached the proposals")
	}
	if len(rejected) != 2 || len(ops) != 1 || ops[0].Candidate.Issue != 1 {
		t.Fatalf("rejected=%d ops=%d", len(rejected), len(ops))
	}

	out := Render([]*model.Candidate{live, plain}, domains, caps, day)
	opsIdx := strings.Index(out, "## Comment opportunities")
	propIdx := strings.Index(out, "## Proposed")
	if opsIdx < 0 || propIdx < 0 || opsIdx < propIdx {
		t.Fatal("comment opportunities are not below the proposals")
	}
	if !strings.Contains(out, "**No competing PR**") {
		t.Error("the warning that these are someone else's work is missing")
	}
	if !strings.Contains(out, "[PR #99](https://pr) by @someone") {
		t.Errorf("the PR link is missing:\n%s", out)
	}
}

// TestEveryQuoteCarriesItsSource is the report's whole contract: the brief is
// model-extracted, so a reader must be able to check the reading in one click.
func TestEveryQuoteCarriesItsSource(t *testing.T) {
	c := cand("kornia/kornia", 4201, model.StatusProposed)
	c.Brief = &model.Brief{
		MaintainerDesiredApproach: "document the limit and raise a clear error",
		ApproachSourceURL:         "https://github.com/kornia/kornia/issues/4201#issuecomment-1",
		ApproachAuthorAssociation: "OWNER",
		RejectedApproaches:        []string{"do not cast to float64 in the hot path"},
		AcceptanceCriteria:        []string{"the MPS test passes"},
		OpenQuestions:             []string{"should it warn or raise?"},
		Reproduction:              "run pytest tests/geometry",
	}
	out := Render([]*model.Candidate{c}, domains, caps, day)
	for _, want := range []string{
		"**Maintainer wants** [OWNER] ([source](https://github.com/kornia/kornia/issues/4201#issuecomment-1))",
		"**Ruled out**", "**Done when**", "**UNRESOLVED**", "**Repro**",
		"pipeline approve kornia__kornia__4201",
		"pipeline reject  kornia__kornia__4201 --reason",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q", want)
		}
	}
}

func TestABriefWithoutASourceStillRenders(t *testing.T) {
	c := cand("some/cli", 1, model.StatusProposed)
	c.Brief = &model.Brief{MaintainerDesiredApproach: "just do X", ApproachAuthorAssociation: "MEMBER"}
	out := Render([]*model.Candidate{c}, domains, caps, day)
	if strings.Contains(out, "([source]())") {
		t.Error("an empty source rendered as a broken link")
	}
	if !strings.Contains(out, "**Maintainer wants** [MEMBER]: \"just do X\"") {
		t.Errorf("out:\n%s", out)
	}
}

func TestNoBriefSaysSo(t *testing.T) {
	out := Render([]*model.Candidate{cand("some/cli", 1, model.StatusProposed)}, domains, caps, day)
	if !strings.Contains(out, "_no brief extracted_") {
		t.Errorf("out:\n%s", out)
	}
	empty := cand("some/cli", 2, model.StatusProposed)
	empty.Brief = &model.Brief{}
	out2 := Render([]*model.Candidate{empty}, domains, caps, day)
	if !strings.Contains(out2, "said nothing extractable") {
		t.Errorf("an empty brief is indistinguishable from a missing one:\n%s", out2)
	}
}

// TestPolicyLineNamesWhatWouldStopAContribution.
func TestPolicyLineNamesWhatWouldStopAContribution(t *testing.T) {
	c := cand("kornia/kornia", 1, model.StatusProposed)
	c.IssueCreatedAt = "2021-09-14T00:00:00Z"
	c.Facts = &model.RepoFacts{Stars: 11346, PrimaryLanguage: "Python",
		RequiresCLA: true, RequiresAIDisclosure: true}
	line := policyLine(c)
	for _, want := range []string{"opened 2021-09", "11,346★", "Python", "no DCO",
		"**CLA required**", "AI disclosure required"} {
		if !strings.Contains(line, want) {
			t.Errorf("policy line %q is missing %q", line, want)
		}
	}
	// A ban is the stronger statement and replaces the disclosure note.
	c.Facts.BansAIPRs = true
	if l := policyLine(c); !strings.Contains(l, "BANS AI PRs") || strings.Contains(l, "disclosure") {
		t.Errorf("= %q", l)
	}
	// No facts must not render a confident-looking line.
	c.Facts = nil
	if l := policyLine(c); l != "policy: unknown" {
		t.Errorf("= %q", l)
	}
}

func TestStalePRTakeoverCreditsTheOriginalAuthor(t *testing.T) {
	c := cand("helm/helm", 13284, model.StatusProposed)
	c.Contest = model.ContestStalePR
	c.PRSignal = &model.PRSignal{Number: 77, URL: "https://pr", Author: "earlier-person",
		Reasons: []string{"no commit for 400d"}}
	out := Render([]*model.Candidate{c}, domains, caps, day)
	if !strings.Contains(out, "Credit @earlier-person in the PR body") {
		t.Errorf("a takeover does not credit the original author:\n%s", out)
	}
	if !strings.Contains(out, "no commit for 400d") {
		t.Error("the reason the PR is considered stale is not shown")
	}
}

func TestEmptyDayIsStated(t *testing.T) {
	out := Render(nil, domains, caps, day)
	if !strings.Contains(out, "Nothing cleared every bar today") {
		t.Errorf("out:\n%s", out)
	}
	if !strings.Contains(out, "0 proposed · 0 rejected") {
		t.Error("the counts line is wrong")
	}
	if !strings.Contains(out, "caps: 2/day, 5 open, 1/repo") {
		t.Error("the caps are not stated")
	}
}

func TestRejectedFallsBackToScoreFailures(t *testing.T) {
	c := cand("some/cli", 1, model.StatusRejected)
	c.ScoreFailures = []string{"a", "b", "c", "d"}
	out := Render([]*model.Candidate{c}, domains, caps, day)
	if !strings.Contains(out, "— a; b; c") || strings.Contains(out, "; d") {
		t.Errorf("want the first three failures only:\n%s", out)
	}
	c2 := cand("some/cli", 2, model.StatusRejected)
	if !strings.Contains(Render([]*model.Candidate{c2}, domains, caps, day), "no reason recorded") {
		t.Error("a rejection with no reason renders blank")
	}
}

// TestGroupingDoesNotReshuffleTheRanking: re-sorting into buckets would undo
// the ranking the page depends on.
func TestGroupingDoesNotReshuffleTheRanking(t *testing.T) {
	cands := []*model.Candidate{
		cand("kornia/kornia", 1, model.StatusProposed),
		cand("some/cli", 2, model.StatusProposed),
		cand("kornia/kornia", 3, model.StatusProposed),
	}
	got, _, _ := Classify(cands, domains)
	gs := groups(got)
	if len(gs) != 2 {
		t.Fatalf("got %d groups, want 2", len(gs))
	}
	if gs[0].label != "Computer vision (weight 3)" || len(gs[0].entries) != 2 {
		t.Fatalf("first group = %q with %d entries", gs[0].label, len(gs[0].entries))
	}
	out := Render(cands, domains, caps, day)
	if strings.Index(out, "Computer vision") > strings.Index(out, "CLI tools") {
		t.Error("the lighter domain was printed first")
	}
}

func TestNilDomainsStillRenders(t *testing.T) {
	out := Render([]*model.Candidate{cand("a/b", 1, model.StatusProposed)}, nil, caps, day)
	if !strings.Contains(out, "Unmatched by any domain") {
		t.Errorf("out:\n%s", out)
	}
}
