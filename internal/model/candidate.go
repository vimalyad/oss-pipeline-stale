package model

import (
	"fmt"
	"strings"
	"time"
)

// PRSignal records why an existing PR was judged active or abandoned.
// Auditable signals only: dates and labels, never an opinion about the code.
type PRSignal struct {
	Number                   int    `json:"number"`
	URL                      string `json:"url"`
	Author                   string `json:"author"`
	IsDraft                  bool   `json:"is_draft"`
	DaysSinceCommit          *int   `json:"days_since_commit"`
	DaysSinceAuthorComment   *int   `json:"days_since_author_comment"`
	DaysSinceChangesReqested *int   `json:"days_since_changes_requested"`
	// Reviewed is whether anyone from the project has reviewed this pull
	// request at all. It is what separates "the author gave up" from "the
	// maintainers have not got to it", which look identical from author
	// silence alone.
	Reviewed      bool     `json:"reviewed"`
	HasStaleLabel bool     `json:"has_stale_label"`
	ChecksFailing bool     `json:"checks_failing"`
	Reasons       []string `json:"reasons"`
}

// Brief is what the issue thread actually asks for -- the spec a patch is
// written against, extracted from the full thread rather than the issue body.
type Brief struct {
	MaintainerDesiredApproach string   `json:"maintainer_desired_approach"`
	ApproachSourceURL         string   `json:"approach_source_url"`
	ApproachAuthorAssociation string   `json:"approach_author_association"`
	RejectedApproaches        []string `json:"rejected_approaches"`
	AcceptanceCriteria        []string `json:"acceptance_criteria"`
	OpenQuestions             []string `json:"open_questions"`
	ClaimedBy                 string   `json:"claimed_by"`
	ClaimedAt                 string   `json:"claimed_at"`
	Reproduction              string   `json:"reproduction"`
}

// RepoFacts are repo-level gates, cached because they change slowly and cost
// API calls (and, for the policy fields, an LLM adjudication).
type RepoFacts struct {
	Repo                 string `json:"repo"`
	Stars                int    `json:"stars"`
	HasContributing      bool   `json:"has_contributing"`
	BansAIPRs            bool   `json:"bans_ai_prs"`
	RequiresAIDisclosure bool   `json:"requires_ai_disclosure"`
	AIPolicyQuote        string `json:"ai_policy_quote"`
	RequiresDCO          bool   `json:"requires_dco"`
	RequiresCLA          bool   `json:"requires_cla"`
	HasTests             bool   `json:"has_tests"`
	PrimaryLanguage      string `json:"primary_language"`
	// Topics is the axis GitHub actually indexes, and the axis domains are
	// expressed in. v1 fetched it with the rest of the repository metadata
	// and discarded it, so every domain match had to be inferred from the
	// language and the name.
	Topics               []string `json:"topics"`
	MergedFirstTimePR90d bool     `json:"merged_first_time_pr_90d"`
	RequiredIssueLabels  []string `json:"required_issue_labels"`
	ForbiddenIssueLabels []string `json:"forbidden_issue_labels"`
	EligibilityQuote     string   `json:"eligibility_quote"`
	FetchedAt            string   `json:"fetched_at"`
}

// HistoryEntry is one state change. `Forced` marks a sanctioned bypass of
// Transitions; v1 had three of these written into files by hand, with no way
// to tell them apart from legitimate edges.
type HistoryEntry struct {
	At     string `json:"at"`
	From   string `json:"from"`
	To     string `json:"to"`
	Note   string `json:"note,omitempty"`
	Forced bool   `json:"forced,omitempty"`
	Actor  string `json:"actor,omitempty"`
}

// Candidate is one issue under consideration, persisted as a single JSON file
// so a stuck run can be read with `cat` and unstuck in a text editor.
//
// Field tags match v1's on-disk names exactly: the Go build must load the
// existing 224 files with no migration step, so both versions can run against
// the same state and have their output compared.
type Candidate struct {
	Repo           string   `json:"repo"`
	Issue          int      `json:"issue"`
	Title          string   `json:"title"`
	URL            string   `json:"url"`
	Status         Status   `json:"status"`
	Labels         []string `json:"labels"`
	Comments       int      `json:"comments"`
	Reactions      int      `json:"reactions"`
	IssueUpdatedAt string   `json:"issue_updated_at"`
	IssueCreatedAt string   `json:"issue_created_at"`
	// LinkedPRs are the open pull requests the issue's timeline points at, as
	// the discovery search saw them. Kept rather than consumed in the same
	// run so triage is a separate command that does not have to ask GitHub
	// again -- and so a contested verdict can be re-read later against the
	// evidence it was actually made on.
	LinkedPRs     []int      `json:"linked_prs,omitempty"`
	Contest       Contest    `json:"contest"`
	PRSignal      *PRSignal  `json:"pr_signal"`
	Brief         *Brief     `json:"brief"`
	Facts         *RepoFacts `json:"facts"`
	ScoreFailures []string   `json:"score_failures"`
	// SoftPenalties do not reject; they rank a candidate below cleaner ones.
	SoftPenalties []string `json:"soft_penalties"`
	// Blockers need a one-off human action (signing a CLA, say) first.
	Blockers      []string         `json:"blockers"`
	RejectReason  string           `json:"reject_reason"`
	Branch        string           `json:"branch"`
	PRNumber      *int             `json:"pr_number"`
	PRURL         string           `json:"pr_url"`
	WatchSeen     []string         `json:"watch_seen"`
	QueuedReplies []map[string]any `json:"queued_replies"`
	DisclosedAI   bool             `json:"disclosed_ai"`
	// TookOver records that this branch actually continued someone else's
	// commits, rather than being cut fresh. The pull request body says one of
	// two different things depending on it, and only one of them is true.
	TookOver bool           `json:"took_over"`
	Credits  string         `json:"credits"`
	History  []HistoryEntry `json:"history"`
}

// Slug is the filename stem and the identifier used everywhere in the CLI.
func (c *Candidate) Slug() string {
	return fmt.Sprintf("%s__%d", strings.ReplaceAll(c.Repo, "/", "__"), c.Issue)
}

func (c *Candidate) Owner() string {
	owner, _, _ := strings.Cut(c.Repo, "/")
	return owner
}

// Transition moves the candidate to a new status, or fails.
//
// It returns ErrIllegalTransition rather than panicking or silently allowing
// the change, because the whole point of the table is that an unattended run
// cannot reach a state it has no business being in.
func Transition(c *Candidate, next Status, note string) error {
	allowed, known := Transitions[c.Status]
	if !known {
		return fmt.Errorf("%w: %s has unknown status %q",
			ErrIllegalTransition, c.Slug(), c.Status)
	}
	if !allowed[next] {
		return fmt.Errorf("%w: %s cannot go %s -> %s (allowed: %s)",
			ErrIllegalTransition, c.Slug(), c.Status, next, allowedList(allowed))
	}
	c.History = append(c.History, HistoryEntry{
		At:   time.Now().UTC().Format("2006-01-02T15:04:05-07:00"),
		From: string(c.Status),
		To:   string(next),
		Note: note,
	})
	c.Status = next
	return nil
}

// ReopenEdges are the only sanctioned bypasses of Transitions. Each exists
// because a human deliberately revisits a terminal decision.
var ReopenEdges = map[[2]Status]string{
	{StatusRejected, StatusProposed}:  "rescore --apply",
	{StatusAbandoned, StatusApproved}: "retry",
}

// Reopen performs a sanctioned bypass, recording who did it and why.
// v1 had these edges hand-written into JSON with fake timestamps; making the
// bypass explicit is what lets `doctor` validate every other edge strictly.
func Reopen(c *Candidate, next Status, actor, reason string) error {
	if _, ok := ReopenEdges[[2]Status{c.Status, next}]; !ok {
		return fmt.Errorf("%w: %s -> %s is not a reopen edge",
			ErrIllegalTransition, c.Status, next)
	}
	c.History = append(c.History, HistoryEntry{
		At:     time.Now().UTC().Format("2006-01-02T15:04:05-07:00"),
		From:   string(c.Status),
		To:     string(next),
		Note:   reason,
		Forced: true,
		Actor:  actor,
	})
	c.Status = next
	return nil
}

func allowedList(m map[Status]bool) string {
	if len(m) == 0 {
		return "none, terminal"
	}
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, string(s))
	}
	sortStrings(out)
	return strings.Join(out, ", ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
