// Package discover sweeps the watchlist for candidate issues.
//
// One search per repository, with the labels OR'd together -- comma-separated
// values are a union in GitHub's search syntax -- rather than one query per
// repository/label pair. Bodies and comment threads are deliberately not
// fetched here; that is internal/harvest, and it only runs on what survives
// the cheap bars.
//
// Two properties matter more than the fetching.
//
// The per-run cap must trim breadth-first. v1 filled it sequentially and
// stopped at the seventh of twenty-five repositories, so the tail of the
// watchlist was never examined at all and the repositories at the end were
// dead weight. Candidates are interleaved instead, so the cap costs depth
// rather than amputating the alphabet.
//
// And each repository's own required labels are added to the search. eslint
// accepts outside pull requests only for issues labelled `accepted`, and
// typescript-eslint only for `accepting prs`; neither is in the generic list.
// Without this the only issues findable in those repositories are the ones
// their own rules make ineligible -- structurally dead targets.
package discover

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vimalyad/osspipeline/internal/model"
)

// API is the GraphQL surface this package needs.
type API interface {
	GraphQL(ctx context.Context, query string, vars map[string]any, v any) error
}

// Facts supplies each repository's own eligibility labels.
type Facts interface {
	LoadRepoFacts(repo string) (*model.RepoFacts, error)
}

// Seen decides whether a candidate is worth looking at again.
type Seen interface {
	ShouldReconsider(slug string, now time.Time, afterDays int) bool
}

const searchQuery = `
query($q:String!, $n:Int!) {
  search(query:$q, type:ISSUE, first:$n) {
    issueCount
    nodes {
      ... on Issue {
        number title url createdAt updatedAt
        repository { nameWithOwner stargazerCount primaryLanguage { name } }
        author { login }
        labels(first:20) { nodes { name } }
        comments { totalCount }
        reactions { totalCount }
        timelineItems(first:30, itemTypes:[CROSS_REFERENCED_EVENT]) {
          nodes {
            ... on CrossReferencedEvent {
              willCloseTarget
              source {
                __typename
                ... on PullRequest {
                  number url state isDraft updatedAt closed merged
                  author { login }
                }
              }
            }
          }
        }
      }
    }
  }
}`

type searchResponse struct {
	Search struct {
		IssueCount int          `json:"issueCount"`
		Nodes      []*issueNode `json:"nodes"`
	} `json:"search"`
}

type issueNode struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Comments      struct{ TotalCount int } `json:"comments"`
	Reactions     struct{ TotalCount int } `json:"reactions"`
	TimelineItems struct {
		Nodes []struct {
			Source *struct {
				Typename string `json:"__typename"`
				Number   int    `json:"number"`
				URL      string `json:"url"`
				State    string `json:"state"`
			} `json:"source"`
		} `json:"nodes"`
	} `json:"timelineItems"`
}

// Found is one candidate plus the open pull requests already referencing it,
// which internal/contest consumes in the same run rather than refetching.
type Found struct {
	Candidate *model.Candidate
	LinkedPRs []LinkedPR
}

type LinkedPR struct {
	Number int
	URL    string
}

// Options tune a sweep.
type Options struct {
	PerRepo int
	// Cap is the most candidates a single run may produce.
	Cap int
	// ReconsiderAfterDays is how long a transient rejection rests.
	ReconsiderAfterDays int
	// Concurrency bounds parallel searches. Bounded on purpose: GitHub's
	// secondary rate limits are partly per-IP, and the user does manual work
	// from another account on the same machine.
	Concurrency int
	Now         func() time.Time
	Log         func(string)
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) logf(f string, a ...any) {
	if o.Log != nil {
		o.Log(fmt.Sprintf(f, a...))
	}
}

// Result is the sweep's outcome, including what it could not read.
type Result struct {
	Found []Found
	// Skipped names repositories whose search failed, with the reason. A
	// sweep that silently returns fewer candidates looks identical to a quiet
	// week, and that is how a broken token went unnoticed for two days.
	Skipped map[string]string
	Swept   int
}

// Query builds the search for one repository.
func Query(repo string, labels []string, f Facts) string {
	wanted := append([]string{}, labels...)
	have := map[string]bool{}
	for _, l := range wanted {
		have[strings.ToLower(l)] = true
	}
	if f != nil {
		if facts, err := f.LoadRepoFacts(repo); err == nil && facts != nil {
			for _, r := range facts.RequiredIssueLabels {
				if !have[strings.ToLower(r)] {
					have[strings.ToLower(r)] = true
					wanted = append(wanted, r)
				}
			}
		}
	}
	quoted := make([]string, len(wanted))
	for i, l := range wanted {
		quoted[i] = `"` + l + `"`
	}
	return fmt.Sprintf("repo:%s is:issue is:open no:assignee label:%s sort:updated-desc",
		repo, strings.Join(quoted, ","))
}

// Sweep searches every repository and interleaves the results.
func Sweep(ctx context.Context, api API, f Facts, seen Seen, repos, labels []string, o Options) Result {
	if o.PerRepo <= 0 {
		o.PerRepo = 10
	}
	if o.Cap <= 0 {
		o.Cap = 25
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 6
	}
	now := o.now()
	res := Result{Skipped: map[string]string{}, Swept: len(repos)}

	buckets := make([][]Found, len(repos))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, o.Concurrency)

	for i, repo := range repos {
		wg.Add(1)
		go func(i int, repo string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var resp searchResponse
			err := api.GraphQL(ctx, searchQuery, map[string]any{
				"q": Query(repo, labels, f), "n": o.PerRepo,
			}, &resp)
			if err != nil {
				mu.Lock()
				res.Skipped[repo] = err.Error()
				mu.Unlock()
				o.logf("  %-45s skipped (%s)", repo, shorten(err.Error(), 60))
				return
			}

			var bucket []Found
			open := 0
			for _, n := range resp.Search.Nodes {
				if n == nil {
					continue
				}
				open++
				c := candidateOf(n)
				if seen != nil && !seen.ShouldReconsider(c.Slug(), now, o.ReconsiderAfterDays) {
					continue
				}
				bucket = append(bucket, Found{Candidate: c, LinkedPRs: openPRs(n)})
			}
			o.logf("  %-45s %3d open  %3d new", repo, open, len(bucket))

			mu.Lock()
			buckets[i] = bucket
			mu.Unlock()
		}(i, repo)
	}
	wg.Wait()

	// Round-robin: one from each repository, then a second from each, and so
	// on, so the cap trims depth rather than the tail of the watchlist.
	for depth := 0; depth < o.PerRepo && len(res.Found) < o.Cap; depth++ {
		for _, b := range buckets {
			if depth < len(b) && len(res.Found) < o.Cap {
				res.Found = append(res.Found, b[depth])
			}
		}
	}

	spread := map[string]bool{}
	for _, fd := range res.Found {
		spread[fd.Candidate.Repo] = true
	}
	o.logf("")
	o.logf("  %d candidates across %d repos (cap %d, swept %d, skipped %d)",
		len(res.Found), len(spread), o.Cap, res.Swept, len(res.Skipped))
	return res
}

func candidateOf(n *issueNode) *model.Candidate {
	var labels []string
	for _, l := range n.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	return &model.Candidate{
		Repo:           n.Repository.NameWithOwner,
		Issue:          n.Number,
		Title:          n.Title,
		URL:            n.URL,
		Labels:         labels,
		Comments:       n.Comments.TotalCount,
		Reactions:      n.Reactions.TotalCount,
		IssueUpdatedAt: n.UpdatedAt,
		IssueCreatedAt: n.CreatedAt,
		Status:         model.StatusDiscovered,
	}
}

// openPRs returns the open pull requests cross-referencing this issue.
//
// Only open ones. A closed or merged pull request referencing an issue is
// history, not competition, and counting it would make every issue that has
// ever been attempted look permanently contested.
func openPRs(n *issueNode) []LinkedPR {
	var out []LinkedPR
	seen := map[int]bool{}
	for _, item := range n.TimelineItems.Nodes {
		s := item.Source
		if s == nil || s.Typename != "PullRequest" || s.State != "OPEN" || seen[s.Number] {
			continue
		}
		seen[s.Number] = true
		out = append(out, LinkedPR{Number: s.Number, URL: s.URL})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
