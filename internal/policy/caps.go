package policy

import (
	"fmt"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/model"
)

// Check returns the reasons target may not become a pull request right now.
// An empty slice means it is clear to proceed.
//
// A breach is not an error. It is the expected way a deliberately low-volume
// pipeline says "enough for today", and every reason is phrased so it can be
// read on a phone without looking anything up.
//
// Time is a parameter rather than time.Now() so the day boundary and the
// cooldown window are testable, and so one run evaluates every cap against a
// single instant.
func (c Caps) Check(all []*model.Candidate, target *model.Candidate, now time.Time) []string {
	var reasons []string

	openNow := 0
	sameRepo := 0
	for _, o := range all {
		if !isOpen(o.Status) {
			continue
		}
		openNow++
		if o.Repo == target.Repo {
			sameRepo++
		}
	}
	if openNow >= c.MaxOpenPRs {
		reasons = append(reasons,
			fmt.Sprintf("%d pull requests already open (cap %d)", openNow, c.MaxOpenPRs))
	}
	if sameRepo >= c.MaxOpenPerRepo {
		reasons = append(reasons, "already have an open pull request on "+target.Repo)
	}

	// Counted from history rather than from the current status, because a PR
	// opened this morning and merged this afternoon still spent today's
	// budget. Counting open PRs instead would let a fast merge refund it.
	today := now.UTC().Format("2006-01-02")
	openedToday := 0
	for _, o := range all {
		for _, h := range o.History {
			if h.To == string(model.StatusPROpen) && strings.HasPrefix(h.At, today) {
				openedToday++
				break
			}
		}
	}
	if openedToday >= c.PRsPerDay {
		reasons = append(reasons,
			fmt.Sprintf("%d pull requests opened today (cap %d)", openedToday, c.PRsPerDay))
	}

	// Two pull requests landing on one organisation within days of each other
	// reads as a campaign whoever triages them did not ask for, however good
	// each one is on its own.
	cooldown := time.Duration(c.OrgCooldownDays) * 24 * time.Hour
	if cooldown > 0 {
		for _, o := range all {
			if o.Owner() != target.Owner() || o.Slug() == target.Slug() {
				continue
			}
			when, ok := lastOpenedAt(o)
			if !ok || now.Sub(when) >= cooldown {
				continue
			}
			reasons = append(reasons, fmt.Sprintf("%s is in cooldown until %s (%s)",
				target.Owner(), when.Add(cooldown).Format("2 Jan"), prRef(o)))
			break
		}
	}
	return reasons
}

func isOpen(s model.Status) bool {
	for _, o := range model.OpenStatuses {
		if s == o {
			return true
		}
	}
	return false
}

// lastOpenedAt is when this candidate most recently became a pull request.
// Most recently, not first: a candidate can reach pr_open more than once, and
// the cooldown is about the latest contact with the organisation.
func lastOpenedAt(c *model.Candidate) (time.Time, bool) {
	var found time.Time
	ok := false
	for _, h := range c.History {
		if h.To != string(model.StatusPROpen) {
			continue
		}
		t, err := time.Parse(time.RFC3339, h.At)
		if err != nil {
			continue
		}
		if !ok || t.After(found) {
			found, ok = t, true
		}
	}
	return found, ok
}

func prRef(c *model.Candidate) string {
	if c.PRNumber != nil {
		return fmt.Sprintf("%s#%d", c.Repo, *c.PRNumber)
	}
	return c.Slug()
}
