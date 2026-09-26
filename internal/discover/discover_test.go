package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vimalyad/osspipeline/internal/model"
)

var now = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

type fakeAPI struct {
	// perRepo maps a repo name to the issue numbers it returns.
	perRepo  map[string][]int
	errFor   map[string]error
	queries  []string
	inFlight int32
	maxSeen  int32
	mu       sync.Mutex
	delay    time.Duration
}

func (f *fakeAPI) GraphQL(_ context.Context, _ string, vars map[string]any, v any) error {
	n := atomic.AddInt32(&f.inFlight, 1)
	for {
		m := atomic.LoadInt32(&f.maxSeen)
		if n <= m || atomic.CompareAndSwapInt32(&f.maxSeen, m, n) {
			break
		}
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	defer atomic.AddInt32(&f.inFlight, -1)

	q, _ := vars["q"].(string)
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()

	repo := repoOf(q)
	if err, ok := f.errFor[repo]; ok {
		return err
	}
	var nodes []any
	for _, num := range f.perRepo[repo] {
		nodes = append(nodes, map[string]any{
			"number": num, "title": fmt.Sprintf("issue %d", num),
			"url":       fmt.Sprintf("https://github.com/%s/issues/%d", repo, num),
			"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-09-01T00:00:00Z",
			"repository":    map[string]any{"nameWithOwner": repo},
			"labels":        map[string]any{"nodes": []any{map[string]any{"name": "bug"}}},
			"comments":      map[string]any{"totalCount": 2},
			"reactions":     map[string]any{"totalCount": 1},
			"timelineItems": map[string]any{"nodes": []any{}},
		})
	}
	b, _ := json.Marshal(map[string]any{
		"search": map[string]any{"issueCount": len(nodes), "nodes": nodes}})
	return json.Unmarshal(b, v)
}

func repoOf(q string) string {
	for _, f := range strings.Fields(q) {
		if r, ok := strings.CutPrefix(f, "repo:"); ok {
			return r
		}
	}
	return ""
}

type fakeFacts struct{ required map[string][]string }

func (f *fakeFacts) LoadRepoFacts(repo string) (*model.RepoFacts, error) {
	r, ok := f.required[repo]
	if !ok {
		return nil, errors.New("no facts")
	}
	return &model.RepoFacts{Repo: repo, RequiredIssueLabels: r}, nil
}

type fakeSeen struct{ skip map[string]bool }

func (f *fakeSeen) ShouldReconsider(slug string, _ time.Time, _ int) bool {
	return !f.skip[slug]
}

// TestTheCapTrimsBreadthFirst is the v1 defect this package was rewritten
// around: the cap was filled sequentially and stopped at the seventh of
// twenty-five repositories, so the tail of the watchlist was never examined and
// the repositories at the end were dead weight.
func TestTheCapTrimsBreadthFirst(t *testing.T) {
	repos := []string{"a/a", "b/b", "c/c", "d/d", "e/e"}
	api := &fakeAPI{perRepo: map[string][]int{}}
	for _, r := range repos {
		api.perRepo[r] = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	}
	res := Sweep(context.Background(), api, nil, nil, repos, []string{"bug"},
		Options{PerRepo: 10, Cap: 5, Now: func() time.Time { return now }})

	if len(res.Found) != 5 {
		t.Fatalf("got %d candidates, want 5", len(res.Found))
	}
	seen := map[string]bool{}
	for _, f := range res.Found {
		seen[f.Candidate.Repo] = true
	}
	if len(seen) != 5 {
		t.Fatalf("a cap of 5 covered only %d repositories: %v", len(seen), seen)
	}
	// Every repository was still queried, even the ones whose candidates the
	// cap excluded: that is what stops the tail being dead weight.
	if len(api.queries) != len(repos) {
		t.Errorf("queried %d of %d repositories", len(api.queries), len(repos))
	}
}

func TestRoundRobinTakesTheFirstFromEachBeforeTheSecond(t *testing.T) {
	repos := []string{"a/a", "b/b"}
	api := &fakeAPI{perRepo: map[string][]int{
		"a/a": {1, 2, 3},
		"b/b": {10, 20, 30},
	}}
	res := Sweep(context.Background(), api, nil, nil, repos, []string{"bug"},
		Options{PerRepo: 3, Cap: 4, Now: func() time.Time { return now }})

	var order []int
	for _, f := range res.Found {
		order = append(order, f.Candidate.Issue)
	}
	want := []int{1, 10, 2, 20}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestARepositorysOwnLabelsAreSearched: eslint accepts outside pull requests
// only for issues labelled `accepted`, and typescript-eslint only for
// `accepting prs`. Without this the only findable issues are ones the project's
// own rules make ineligible.
func TestARepositorysOwnLabelsAreSearched(t *testing.T) {
	f := &fakeFacts{required: map[string][]string{
		"eslint/eslint": {"accepted"},
		"cli/cli":       {"help wanted"}, // already in the generic list
	}}
	generic := []string{"good first issue", "help wanted"}

	q := Query("eslint/eslint", generic, f)
	for _, want := range []string{`"good first issue"`, `"help wanted"`, `"accepted"`,
		"repo:eslint/eslint", "is:issue", "is:open", "no:assignee", "sort:updated-desc"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q is missing %q", q, want)
		}
	}
	// A required label already present must not be duplicated: GitHub treats a
	// repeated qualifier value as a narrower filter, not a wider one.
	q2 := Query("cli/cli", generic, f)
	if strings.Count(q2, `"help wanted"`) != 1 {
		t.Errorf("duplicated label: %q", q2)
	}
	// Case differences are not new labels either.
	q3 := Query("x/y", []string{"Help Wanted"}, &fakeFacts{required: map[string][]string{"x/y": {"help wanted"}}})
	if strings.Count(strings.ToLower(q3), "help wanted") != 1 {
		t.Errorf("case-differing duplicate: %q", q3)
	}
}

func TestMissingFactsDoNotBreakTheQuery(t *testing.T) {
	q := Query("new/repo", []string{"bug"}, &fakeFacts{})
	if !strings.Contains(q, `label:"bug"`) {
		t.Fatalf("q = %q", q)
	}
	if q2 := Query("new/repo", []string{"bug"}, nil); q2 != q {
		t.Errorf("a nil Facts changed the query: %q vs %q", q2, q)
	}
}

// TestASkippedRepositoryIsReported: a sweep that silently returns fewer
// candidates looks identical to a quiet week, which is how a broken token went
// unnoticed for two days.
func TestASkippedRepositoryIsReported(t *testing.T) {
	repos := []string{"a/a", "b/b", "c/c"}
	api := &fakeAPI{
		perRepo: map[string][]int{"a/a": {1}, "c/c": {3}},
		errFor:  map[string]error{"b/b": errors.New("HTTP 401: bad credentials")},
	}
	var logged []string
	res := Sweep(context.Background(), api, nil, nil, repos, []string{"bug"},
		Options{PerRepo: 5, Cap: 10, Now: func() time.Time { return now },
			Log: func(s string) { logged = append(logged, s) }})

	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped["b/b"], "401") {
		t.Fatalf("skipped = %v", res.Skipped)
	}
	if len(res.Found) != 2 {
		t.Errorf("one failure cost %d candidates", 2-len(res.Found))
	}
	if res.Swept != 3 {
		t.Errorf("swept = %d, want 3", res.Swept)
	}
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "skipped 1") {
		t.Errorf("the summary hides the skip:\n%s", joined)
	}
}

// TestOnlyOpenPullRequestsCount: a closed or merged PR referencing an issue is
// history, not competition, and counting it would make every issue that has
// ever been attempted look permanently contested.
func TestOnlyOpenPullRequestsCount(t *testing.T) {
	n := &issueNode{}
	raw := `{"nodes":[
	  {"source":{"__typename":"PullRequest","number":1,"state":"OPEN","url":"u1"}},
	  {"source":{"__typename":"PullRequest","number":2,"state":"CLOSED","url":"u2"}},
	  {"source":{"__typename":"PullRequest","number":3,"state":"MERGED","url":"u3"}},
	  {"source":{"__typename":"PullRequest","number":1,"state":"OPEN","url":"u1"}},
	  {"source":{"__typename":"Issue","number":4,"state":"OPEN","url":"u4"}}
	]}`
	if err := json.Unmarshal([]byte(raw), &n.TimelineItems); err != nil {
		t.Fatal(err)
	}
	got := openPRs(n)
	if len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("= %+v, want only the open PR #1 once", got)
	}
}

func TestAlreadySeenCandidatesAreSkipped(t *testing.T) {
	api := &fakeAPI{perRepo: map[string][]int{"a/a": {1, 2, 3}}}
	seen := &fakeSeen{skip: map[string]bool{"a__a__2": true}}
	res := Sweep(context.Background(), api, nil, seen, []string{"a/a"}, []string{"bug"},
		Options{PerRepo: 5, Cap: 10, Now: func() time.Time { return now }})
	for _, f := range res.Found {
		if f.Candidate.Issue == 2 {
			t.Fatal("a candidate the store said to skip came back")
		}
	}
	if len(res.Found) != 2 {
		t.Fatalf("got %d, want 2", len(res.Found))
	}
}

// TestConcurrencyIsBounded: GitHub's secondary rate limits are partly per-IP,
// and the user does manual work from another account on the same machine.
func TestConcurrencyIsBounded(t *testing.T) {
	var repos []string
	perRepo := map[string][]int{}
	for i := 0; i < 24; i++ {
		r := fmt.Sprintf("r%02d/x", i)
		repos = append(repos, r)
		perRepo[r] = []int{1}
	}
	api := &fakeAPI{perRepo: perRepo, delay: 5 * time.Millisecond}
	Sweep(context.Background(), api, nil, nil, repos, []string{"bug"},
		Options{PerRepo: 1, Cap: 100, Concurrency: 4, Now: func() time.Time { return now }})

	if got := atomic.LoadInt32(&api.maxSeen); got > 4 {
		t.Fatalf("%d searches ran at once with a limit of 4", got)
	}
	if got := atomic.LoadInt32(&api.maxSeen); got < 2 {
		t.Fatalf("peak concurrency %d; the sweep ran sequentially", got)
	}
	if len(api.queries) != 24 {
		t.Errorf("queried %d of 24", len(api.queries))
	}
}

// TestOrderIsDeterministicDespiteConcurrency: the candidates a run proposes
// must not depend on which search returned first, or two runs over unchanged
// data would disagree.
func TestOrderIsDeterministicDespiteConcurrency(t *testing.T) {
	repos := []string{"a/a", "b/b", "c/c", "d/d"}
	perRepo := map[string][]int{"a/a": {1, 2}, "b/b": {10, 20}, "c/c": {100}, "d/d": {1000, 2000}}
	var first []int
	for run := 0; run < 12; run++ {
		api := &fakeAPI{perRepo: perRepo, delay: time.Duration(run%3) * time.Millisecond}
		res := Sweep(context.Background(), api, nil, nil, repos, []string{"bug"},
			Options{PerRepo: 2, Cap: 5, Concurrency: 4, Now: func() time.Time { return now }})
		var got []int
		for _, f := range res.Found {
			got = append(got, f.Candidate.Issue)
		}
		if run == 0 {
			first = got
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(first) {
			t.Fatalf("run %d gave %v, run 0 gave %v", run, got, first)
		}
	}
}

func TestCandidateFieldsAreCarried(t *testing.T) {
	api := &fakeAPI{perRepo: map[string][]int{"kornia/kornia": {4201}}}
	res := Sweep(context.Background(), api, nil, nil, []string{"kornia/kornia"}, []string{"bug"},
		Options{PerRepo: 1, Cap: 1, Now: func() time.Time { return now }})
	if len(res.Found) != 1 {
		t.Fatal("no candidate")
	}
	c := res.Found[0].Candidate
	if c.Slug() != "kornia__kornia__4201" || c.Status != model.StatusDiscovered {
		t.Fatalf("c = %+v", c)
	}
	if c.Comments != 2 || c.Reactions != 1 || c.IssueUpdatedAt == "" || len(c.Labels) != 1 {
		t.Errorf("metadata lost: %+v", c)
	}
}

func TestNoReposIsNotAnError(t *testing.T) {
	res := Sweep(context.Background(), &fakeAPI{}, nil, nil, nil, []string{"bug"}, Options{})
	if len(res.Found) != 0 || res.Swept != 0 {
		t.Fatalf("res = %+v", res)
	}
}
