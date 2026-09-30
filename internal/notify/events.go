package notify

import (
	"fmt"
	"strings"

	"github.com/vimalyad/osspipeline/internal/text"
)

// The escalation taxonomy.
//
// The rule: push when the pipeline has stopped and cannot continue without
// the user, or when an outcome is final. Everything else waits for the daily
// digest. An hourly watcher that pushes everything trains the user to ignore
// the phone, which costs more than it saves.
const (
	KindPRMerged          = "pr_merged"
	KindPRClosed          = "pr_closed"
	KindMaintainerReplied = "maintainer_replied"
	KindChangesRequested  = "changes_requested"
	KindCIFailedReal      = "ci_failed_real"
	KindCIFailedInfra     = "ci_failed_infra"
	KindCIFixPushed       = "ci_fix_pushed"
	KindCIFixExhausted    = "ci_fix_exhausted"
	KindNeedsApproval     = "needs_approval"
	KindBlockedNeedsHuman = "blocked_needs_human"
	KindInternalError     = "internal_error"
	KindHalted            = "halted"
	KindPROpened          = "pr_opened"
	KindAutoPROpened      = "auto_pr_opened"
	KindDigest            = "daily_digest"
)

// Immediate reports whether a kind earns a push rather than a digest line.
func Immediate(kind string) bool {
	switch kind {
	case KindPRMerged, KindPRClosed, KindMaintainerReplied, KindChangesRequested,
		KindCIFailedReal, KindCIFixExhausted, KindBlockedNeedsHuman,
		KindInternalError, KindHalted, KindAutoPROpened:
		return true
	}
	// needs_approval is deliberately NOT immediate: nothing is waiting on it,
	// the candidate keeps, and one question at a time is the whole design.
	return false
}

// Opened announces a pull request this pipeline just put in front of
// maintainers. Not immediate by the rule above -- nothing is waiting on the
// user -- but it earns a push anyway, because it is the one moment where
// something irreversible has happened under their name and the digest is
// hours away. Priority stays default so quiet hours still hold it.
func Opened(slug, repo string, issue int, prURL, title string) Event {
	return Event{
		Kind: KindPROpened, Slug: slug, Priority: PriorityDefault,
		Title:     fmt.Sprintf("Opened: %s#%d", repo, issue),
		Body:      title,
		URL:       prURL,
		Tags:      []string{"rocket"},
		DedupeKey: "pr_opened:" + slug,
		Actions:   []Action{{Label: "Open PR", Verb: "view", URL: prURL}},
	}
}

// Merged announces the outcome the entire pipeline exists to produce.
func Merged(slug, repo string, issue int, prURL string) Event {
	return Event{
		Kind: KindPRMerged, Slug: slug, Priority: PriorityHigh,
		Title:     fmt.Sprintf("Merged: %s#%d", repo, issue),
		Body:      "Your pull request was merged.",
		URL:       prURL,
		Tags:      []string{"tada"},
		DedupeKey: "pr_merged:" + slug,
		Actions:   []Action{{Label: "Open PR", Verb: "view", URL: prURL}},
	}
}

func Closed(slug, repo string, issue int, prURL string) Event {
	return Event{
		Kind: KindPRClosed, Slug: slug, Priority: PriorityHigh,
		Title:     fmt.Sprintf("Closed without merging: %s#%d", repo, issue),
		Body:      "The maintainers closed it. Worth reading why before the next attempt.",
		URL:       prURL,
		Tags:      []string{"x"},
		DedupeKey: "pr_closed:" + slug,
		Actions:   []Action{{Label: "Open PR", Verb: "view", URL: prURL}},
	}
}

// ReplyNeeded carries the full draft, because the user approves it from a lock
// screen and must be able to read what they are sending before they send it.
func ReplyNeeded(slug, repo string, issue int, prURL, author, draft string, index int) Event {
	body := draft
	if body == "" {
		body = "(no draft yet)"
	}
	return Event{
		Kind: KindMaintainerReplied, Slug: slug, Priority: PriorityHigh,
		Title: fmt.Sprintf("%s replied on %s#%d", author, repo, issue),
		Body:  body,
		URL:   prURL,
		Tags:  []string{"speech_balloon"},
		// Keyed on the draft so a revised answer notifies again.
		DedupeKey: fmt.Sprintf("reply:%s:%d:%s", slug, index, shortHash(draft)),
		Actions: []Action{
			{Label: "Post", Verb: "reply-post", Arg: fmt.Sprintf("%s %d", slug, index)},
			{Label: "Skip", Verb: "reply-skip", Arg: fmt.Sprintf("%s %d", slug, index)},
			{Label: "Open PR", Verb: "view", URL: prURL},
		},
	}
}

// ApprovalNeeded is the "work on this issue?" decision, reduced to yes or no.
//
// v1 printed four candidates and asked the user to compare them, which is a
// laptop task. The pipeline ranks them and asks about one.
func ApprovalNeeded(slug, repo string, issue int, issueURL, title, wanted, plan, why string) Event {
	body := strings.Join([]string{
		title,
		"",
		"Maintainer asked for: " + oneLine(wanted),
		"We would: " + oneLine(plan),
		"Likely to land because: " + oneLine(why),
	}, "\n")
	return Event{
		Kind: KindNeedsApproval, Slug: slug, Priority: PriorityDefault,
		Title:     fmt.Sprintf("Work on %s#%d?", repo, issue),
		Body:      body,
		URL:       issueURL,
		Tags:      []string{"question"},
		DedupeKey: "needs_approval:" + slug,
		Actions: []Action{
			{Label: "Ship it", Verb: "approve", Arg: slug},
			{Label: "Skip", Verb: "reject", Arg: slug},
			{Label: "Read issue", Verb: "view", URL: issueURL},
		},
	}
}

// CIFailed is only for a failure that is genuinely about the code. An
// infrastructure failure is not the user's problem and must not reach them.
func CIFailed(slug, repo string, issue int, prURL, check, headSHA, why string) Event {
	return Event{
		Kind: KindCIFailedReal, Slug: slug, Priority: PriorityDefault,
		Title:     fmt.Sprintf("CI failing on %s#%d", repo, issue),
		Body:      fmt.Sprintf("%s\n\n%s", check, oneLine(why)),
		URL:       prURL,
		Tags:      []string{"rotating_light"},
		DedupeKey: fmt.Sprintf("ci_failed:%s:%s:%s", slug, short(headSHA, 8), check),
	}
}

// CIFixExhausted fires when automatic fixing gave up. The cap is low because
// repeated pushes to someone else's PR read as spam.
func CIFixExhausted(slug, repo string, issue int, prURL, check string, attempts int) Event {
	return Event{
		Kind: KindCIFixExhausted, Slug: slug, Priority: PriorityHigh,
		Title: fmt.Sprintf("Cannot fix CI on %s#%d", repo, issue),
		Body: fmt.Sprintf("%s is still failing after %d attempt(s). "+
			"Stopping rather than pushing again.", check, attempts),
		URL:       prURL,
		Tags:      []string{"warning"},
		DedupeKey: fmt.Sprintf("ci_exhausted:%s:%s", slug, check),
	}
}

// InternalError is the highest priority because silent failure is this
// project's recurring mode: a swallowed state-machine error would have lost
// the first merge without anyone noticing.
func InternalError(slug, title, detail string) Event {
	return Event{
		Kind: KindInternalError, Slug: slug, Priority: PriorityUrgent,
		Title:     "Pipeline problem: " + title,
		Body:      detail,
		Tags:      []string{"skull"},
		DedupeKey: "internal:" + slug + ":" + shortHash(detail),
		Actions:   []Action{{Label: "Stop everything", Verb: "halt", Arg: "from notification"}},
	}
}

func BlockedNeedsHuman(slug, repo, what, how string) Event {
	return Event{
		Kind: KindBlockedNeedsHuman, Slug: slug, Priority: PriorityDefault,
		Title:     "Blocked: " + repo,
		Body:      what + "\n\n" + how,
		Tags:      []string{"construction"},
		DedupeKey: "blocked:" + repo + ":" + shortHash(what),
	}
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return text.Clip(s, n)
}

// shortHash is a cheap content key for dedupe. Not security-relevant.
func shortHash(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}
