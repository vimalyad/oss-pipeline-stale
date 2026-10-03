// Package contest answers one question: is this issue already being worked on?
//
// The bias is deliberately conservative. A pull request is treated as ACTIVE
// unless it is provably abandoned, because opening a competing pull request
// against someone's live work is the most reliably resented thing an outside
// contributor can do, and the cost lands on the user's account rather than on
// this code. Ambiguity resolves to "leave it alone".
//
// Nothing here judges anyone's code. Classification uses dates, labels and
// review states only. The ranking of stale pull requests exists to choose
// between abandoned ones, never to justify contesting live work.
package contest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/policy"
)

// API is the GraphQL surface this package needs.
type API interface {
	GraphQL(ctx context.Context, query string, vars map[string]any, v any) error
}

// staleLabelHints are substrings maintainers use to mark a pull request as
// no longer moving.
var staleLabelHints = []string{"stale", "abandoned", "inactive", "no-response", "needs-rebase"}

const prDetailQuery = `
query($owner:String!, $name:String!, $number:Int!) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$number) {
      number url isDraft state createdAt updatedAt
      author { login }
      labels(first:20) { nodes { name } }
      commits(last:1) {
        nodes { commit { committedDate statusCheckRollup { state } } }
      }
      comments(last:30) { nodes { createdAt author { login } } }
      reviews(last:30) { nodes { state createdAt author { login } } }
    }
  }
}`

type prDetail struct {
	Repository struct {
		PullRequest struct {
			Number    int    `json:"number"`
			URL       string `json:"url"`
			IsDraft   bool   `json:"isDraft"`
			State     string `json:"state"`
			CreatedAt string `json:"createdAt"`
			UpdatedAt string `json:"updatedAt"`
			Author    *struct {
				Login string `json:"login"`
			} `json:"author"`
			Labels struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"labels"`
			Commits struct {
				Nodes []struct {
					Commit struct {
						CommittedDate     string `json:"committedDate"`
						StatusCheckRollup *struct {
							State string `json:"state"`
						} `json:"statusCheckRollup"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
			Comments struct {
				Nodes []struct {
					CreatedAt string `json:"createdAt"`
					Author    *struct {
						Login string `json:"login"`
					} `json:"author"`
				} `json:"nodes"`
			} `json:"comments"`
			Reviews struct {
				Nodes []struct {
					State     string `json:"state"`
					CreatedAt string `json:"createdAt"`
					Author    *struct {
						Login string `json:"login"`
					} `json:"author"`
				} `json:"nodes"`
			} `json:"reviews"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// LinkedPR is a pull request that references the issue.
type LinkedPR struct {
	Number int
	URL    string
}

// Signal fetches the auditable facts about one pull request.
func Signal(ctx context.Context, api API, repo string, number int, now time.Time) (*model.PRSignal, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("contest: %q is not owner/name", repo)
	}
	var resp prDetail
	if err := api.GraphQL(ctx, prDetailQuery, map[string]any{
		"owner": owner, "name": name, "number": number,
	}, &resp); err != nil {
		return nil, fmt.Errorf("contest %s#%d: %w", repo, number, err)
	}
	pr := resp.Repository.PullRequest

	author := ""
	if pr.Author != nil {
		author = pr.Author.Login
	}

	var committed string
	var rollup string
	if len(pr.Commits.Nodes) > 0 {
		c := pr.Commits.Nodes[0].Commit
		committed = c.CommittedDate
		if c.StatusCheckRollup != nil {
			rollup = c.StatusCheckRollup.State
		}
	}

	var lastAuthorComment string
	for _, c := range pr.Comments.Nodes {
		if c.Author == nil || c.Author.Login != author || author == "" {
			continue
		}
		if c.CreatedAt > lastAuthorComment {
			lastAuthorComment = c.CreatedAt
		}
	}
	var lastChangesRequested string
	reviewed := false
	for _, r := range pr.Reviews.Nodes {
		// A review by the author is not the project engaging with the work.
		if r.Author != nil && r.Author.Login == author && author != "" {
			continue
		}
		// PENDING is an unsubmitted draft review, visible only to its writer.
		if r.State != "" && r.State != "PENDING" {
			reviewed = true
		}
		if r.State == "CHANGES_REQUESTED" && r.CreatedAt > lastChangesRequested {
			lastChangesRequested = r.CreatedAt
		}
	}

	var labels []string
	for _, l := range pr.Labels.Nodes {
		labels = append(labels, strings.ToLower(l.Name))
	}

	return &model.PRSignal{
		Number:                   pr.Number,
		URL:                      pr.URL,
		Author:                   author,
		IsDraft:                  pr.IsDraft,
		DaysSinceCommit:          daysSince(committed, now),
		DaysSinceAuthorComment:   daysSince(lastAuthorComment, now),
		DaysSinceChangesReqested: daysSince(lastChangesRequested, now),
		Reviewed:                 reviewed,
		HasStaleLabel:            hasStaleLabel(labels),
		ChecksFailing:            rollup == "FAILURE" || rollup == "ERROR",
	}, nil
}

// ClassifyOne decides whether a single pull request makes an issue off limits.
func ClassifyOne(sig *model.PRSignal, st policy.Staleness) (model.Contest, []string) {
	// Any recent author activity means live work. Checked first and wins
	// outright: nothing below can talk us into contesting it.
	best := -1
	for _, d := range []*int{sig.DaysSinceCommit, sig.DaysSinceAuthorComment} {
		if d != nil && *d <= st.ActivePRDays && (best < 0 || *d < best) {
			best = *d
		}
	}
	if best >= 0 {
		return model.ContestActivePR, []string{fmt.Sprintf(
			"author active %dd ago (within %dd window)", best, st.ActivePRDays)}
	}

	var reasons []string
	if sig.HasStaleLabel {
		reasons = append(reasons, "carries a stale/abandoned label")
	}
	// Author silence means abandonment only once the project has looked at
	// the work. Before that it means the author finished and is waiting, and
	// astral-sh/ruff#23140 is the case: it implements exactly what a MEMBER
	// asked for on the issue, has never been reviewed, and went quiet five
	// months ago. By author silence alone it classified as stale and the
	// issue became a proposal -- and a competing pull request would have
	// taken credit for someone else's unreviewed contribution while adding to
	// the same queue nobody is reading.
	//
	// Past UnreviewedSilentDays it counts anyway. A pull request nobody has
	// reviewed in that long is not a backlog entry, and its author is gone.
	// Derived, not just read off the signal. Changes having been requested is
	// a review by definition, and a signal that says one without the other is
	// inconsistent -- which a hand-built one, or one stored before this field
	// existed, can easily be. Deciding it here means the rule cannot be
	// weakened by the shape of its input.
	reviewed := sig.Reviewed || sig.DaysSinceChangesReqested != nil
	silentDays := st.AuthorSilentDays
	if !reviewed {
		silentDays = st.UnreviewedSilentDays
	}
	if silentDays > 0 && sig.DaysSinceCommit != nil && *sig.DaysSinceCommit > silentDays {
		why := fmt.Sprintf("no commit for %dd", *sig.DaysSinceCommit)
		if !reviewed {
			why += " and never reviewed"
		}
		reasons = append(reasons, why)
	}
	if sig.DaysSinceChangesReqested != nil && *sig.DaysSinceChangesReqested > st.ChangesRequestedDays {
		// Unanswered means the author has not spoken since the request. An
		// author who replied and is waiting on the maintainer is not stalling.
		unanswered := sig.DaysSinceAuthorComment == nil ||
			*sig.DaysSinceAuthorComment > *sig.DaysSinceChangesReqested
		if unanswered {
			reasons = append(reasons, fmt.Sprintf(
				"changes requested %dd ago, unanswered", *sig.DaysSinceChangesReqested))
		}
	}
	if sig.ChecksFailing && sig.DaysSinceCommit != nil && *sig.DaysSinceCommit > st.CIRedUntouchedDays {
		reasons = append(reasons, fmt.Sprintf(
			"CI red and untouched for %dd", *sig.DaysSinceCommit))
	}
	if len(reasons) > 0 {
		return model.ContestStalePR, reasons
	}

	// Between the windows with no positive staleness signal. Silence is not
	// evidence of abandonment, so this stays off limits.
	return model.ContestActivePR, []string{
		"no activity in the active window, but no staleness signal either -- " +
			"treated as live (conservative default)"}
}

// Classify decides for a whole issue, given every pull request linked to it.
func Classify(ctx context.Context, api API, repo string, prs []LinkedPR, st policy.Staleness, now time.Time) (model.Contest, *model.PRSignal, error) {
	if len(prs) == 0 {
		return model.ContestNoPR, nil, nil
	}

	type scored struct {
		sig     *model.PRSignal
		verdict model.Contest
		reasons []string
	}
	var all []scored
	for _, pr := range prs {
		sig, err := Signal(ctx, api, repo, pr.Number, now)
		if err != nil {
			// Unknown state is not evidence of abandonment. An unreadable
			// pull request makes the issue contested, not available.
			return model.ContestActivePR, nil, nil
		}
		v, reasons := ClassifyOne(sig, st)
		all = append(all, scored{sig, v, reasons})
	}

	// One live pull request is enough to put the issue off limits.
	for _, s := range all {
		if s.verdict == model.ContestActivePR {
			s.sig.Reasons = s.reasons
			return model.ContestActivePR, s.sig, nil
		}
	}

	// All stale: surface the most abandoned one, by longest silence.
	bestIdx := 0
	for i, s := range all {
		if days(s.sig.DaysSinceCommit) > days(all[bestIdx].sig.DaysSinceCommit) {
			bestIdx = i
		}
	}
	all[bestIdx].sig.Reasons = all[bestIdx].reasons
	return model.ContestStalePR, all[bestIdx].sig, nil
}

func days(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// daysSince returns nil for an absent or unparseable timestamp, which every
// caller must treat as "unknown" rather than as zero. A missing date read as
// "0 days ago" would make an abandoned pull request look active; read as
// "very old" it would make a live one look abandoned. Only nil is honest.
func daysSince(iso string, now time.Time) *int {
	if strings.TrimSpace(iso) == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return nil
	}
	d := int(now.UTC().Sub(t).Hours() / 24)
	if d < 0 {
		d = 0
	}
	return &d
}

func hasStaleLabel(labels []string) bool {
	for _, l := range labels {
		for _, h := range staleLabelHints {
			if strings.Contains(l, h) {
				return true
			}
		}
	}
	return false
}
