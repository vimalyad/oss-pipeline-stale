// Package propose renders the daily report, which is the human gate's only
// input.
//
// Every claim links back to the comment it came from, so the reading can be
// checked rather than trusted. That matters because the brief is extracted by a
// model, and a misread thread is the failure mode most likely to produce a bad
// pull request -- one that costs a maintainer their review time and reads as
// careless.
//
// Ranking is by domain weight, which is what makes the profile a steering wheel
// rather than a description: raising one number moves a whole area up the page,
// with no code change.
package propose

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/profile"
	"github.com/vimalyad/osspipeline/internal/text"
)

// Domains resolves which area a candidate belongs to, for weighting and
// grouping. Declared here so propose needs nothing else from the profile.
type Domains interface {
	ForRepo(repo, language string, topics []string) (profile.Domain, bool)
}

// Entry is one candidate with the domain that earned it its place.
type Entry struct {
	Candidate *model.Candidate
	Domain    profile.Domain
	HasDomain bool
}

// Rank orders the proposals.
//
// Unblocked work first, because a blocker needs a human action outside the
// pipeline and nothing can be approved until it clears. Then heavier domains,
// then fewer soft penalties, then more reactions -- and finally the slug, so
// two runs over unchanged state produce the same page rather than an arbitrary
// reordering that reads as movement.
func Rank(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if ab, bb := len(a.Candidate.Blockers) > 0, len(b.Candidate.Blockers) > 0; ab != bb {
			return !ab
		}
		if wa, wb := weight(a), weight(b); wa != wb {
			return wa > wb
		}
		if la, lb := len(a.Candidate.SoftPenalties), len(b.Candidate.SoftPenalties); la != lb {
			return la < lb
		}
		if a.Candidate.Reactions != b.Candidate.Reactions {
			return a.Candidate.Reactions > b.Candidate.Reactions
		}
		return a.Candidate.Slug() < b.Candidate.Slug()
	})
}

// weight treats an unmatched domain as 0, below every declared one. A candidate
// nothing in the profile claims is not what the user asked for, so it ranks
// under everything that is.
func weight(e Entry) int {
	if !e.HasDomain {
		return 0
	}
	if e.Domain.Weight < 1 {
		return 1
	}
	return e.Domain.Weight
}

// Classify splits candidates into the three groups the page needs and attaches
// each one's domain.
func Classify(cands []*model.Candidate, d Domains) (proposed, rejected, commentOps []Entry) {
	for _, c := range cands {
		e := Entry{Candidate: c}
		if d != nil {
			lang, topics := "", []string(nil)
			if c.Facts != nil {
				lang, topics = c.Facts.PrimaryLanguage, c.Facts.Topics
			}
			e.Domain, e.HasDomain = d.ForRepo(c.Repo, lang, topics)
		}
		switch c.Status {
		case model.StatusProposed:
			proposed = append(proposed, e)
		case model.StatusRejected:
			rejected = append(rejected, e)
			// Someone else's live work is not a target, but it can still be
			// somewhere a useful comment lands. Kept separate from the
			// proposals so the distinction cannot blur.
			if c.Contest == model.ContestActivePR && c.PRSignal != nil {
				commentOps = append(commentOps, e)
			}
		}
	}
	Rank(proposed)
	sort.SliceStable(rejected, func(i, j int) bool {
		return rejected[i].Candidate.Slug() < rejected[j].Candidate.Slug()
	})
	sort.SliceStable(commentOps, func(i, j int) bool {
		return commentOps[i].Candidate.Slug() < commentOps[j].Candidate.Slug()
	})
	return proposed, rejected, commentOps
}

// Render produces the report.
func Render(cands []*model.Candidate, d Domains, caps policy.Caps, day time.Time) string {
	proposed, rejected, commentOps := Classify(cands, d)

	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }

	p("# Contribution proposals — %s", day.Format("2006-01-02"))
	p("")
	p("%d proposed · %d rejected · caps: %d/day, %d open, %d/repo",
		len(proposed), len(rejected), caps.PRsPerDay, caps.MaxOpenPRs, caps.MaxOpenPerRepo)
	p("")
	p("Approve only what you could defend in a review conversation with the " +
		"maintainer. Every quote below links to its source comment — check the " +
		"reading, don't trust it.")
	p("")

	p("## Proposed")
	p("")
	if len(proposed) == 0 {
		p("_Nothing cleared every bar today._")
		p("")
	}
	// Grouped by domain, in the order Rank already put them, so the heaviest
	// area appears first and the grouping does not reshuffle the ranking.
	for _, g := range groups(proposed) {
		if len(proposed) > 0 && g.label != "" {
			p("### %s", g.label)
			p("")
		}
		for _, e := range g.entries {
			renderOne(p, e)
		}
	}

	if len(commentOps) > 0 {
		p("## Comment opportunities")
		p("")
		p("Active pull requests where a constructive comment may help. " +
			"**No competing PR** — these are someone else's live work.")
		p("")
		for _, e := range commentOps {
			s := e.Candidate.PRSignal
			p("- %s#%d → [PR #%d](%s) by @%s (%s)",
				e.Candidate.Repo, e.Candidate.Issue, s.Number, s.URL, s.Author,
				strings.Join(s.Reasons, "; "))
		}
		p("")
	}

	if len(rejected) > 0 {
		p("## Rejected")
		p("")
		p("Reasons are the tuning signal — if a class of rejection looks wrong, " +
			"the scorer or the watchlist is what needs changing.")
		p("")
		for _, e := range rejected {
			p("- **%s#%d** — %s", e.Candidate.Repo, e.Candidate.Issue, why(e.Candidate))
		}
		p("")
	}
	return b.String()
}

func renderOne(p func(string, ...any), e Entry) {
	c := e.Candidate
	p("#### %s#%d — %s", c.Repo, c.Issue, c.Title)
	p("")
	p("%s · `%s` · %s", c.URL, orElse(string(c.Contest), "unknown"), policyLine(c))
	p("")
	for _, blk := range c.Blockers {
		p("> **Blocked:** %s", blk)
		p("")
	}
	if len(c.SoftPenalties) > 0 {
		p("_Ranked lower: %s_", strings.Join(c.SoftPenalties, "; "))
		p("")
	}
	if c.Contest == model.ContestStalePR && c.PRSignal != nil {
		s := c.PRSignal
		p("**Taking over a stale PR**: [#%d](%s) by @%s — %s.",
			s.Number, s.URL, s.Author, strings.Join(s.Reasons, "; "))
		p("Credit @%s in the PR body and build on their commits where usable.", s.Author)
		p("")
	}
	for _, line := range briefBlock(c) {
		p("%s", line)
	}
	p("")
	p("```")
	p("pipeline approve %s", c.Slug())
	p("pipeline reject  %s --reason \"...\"", c.Slug())
	p("```")
	p("")
}

// briefBlock is where the linking matters. Each claim carries the comment it
// came from so a reader can check the extraction in one click, which is the
// only defence against a confidently misread thread.
func briefBlock(c *model.Candidate) []string {
	bf := c.Brief
	if bf == nil {
		return []string{"- _no brief extracted_"}
	}
	var out []string
	if bf.MaintainerDesiredApproach != "" {
		src := ""
		if bf.ApproachSourceURL != "" {
			src = fmt.Sprintf(" ([source](%s))", bf.ApproachSourceURL)
		}
		out = append(out, fmt.Sprintf("- **Maintainer wants** [%s]%s: %q",
			bf.ApproachAuthorAssociation, src, text.Clip(bf.MaintainerDesiredApproach, 400)))
	}
	for _, r := range bf.RejectedApproaches {
		out = append(out, fmt.Sprintf("- **Ruled out**: %q", text.Clip(r, 250)))
	}
	for _, a := range bf.AcceptanceCriteria {
		out = append(out, fmt.Sprintf("- **Done when**: %s", text.Clip(a, 250)))
	}
	for _, q := range bf.OpenQuestions {
		out = append(out, fmt.Sprintf("- **UNRESOLVED**: %s", text.Clip(q, 250)))
	}
	if bf.ClaimedBy != "" {
		out = append(out, fmt.Sprintf("- **Claimed by** @%s %s", bf.ClaimedBy, bf.ClaimedAt))
	}
	if bf.Reproduction != "" {
		out = append(out, fmt.Sprintf("- **Repro**: %s", text.Clip(bf.Reproduction, 250)))
	}
	if len(out) == 0 {
		return []string{"- _brief is empty -- the thread said nothing extractable_"}
	}
	return out
}

// policyLine states the repository facts that decide whether a contribution is
// even accepted, in the order they matter to a human deciding.
func policyLine(c *model.Candidate) string {
	f := c.Facts
	if f == nil {
		return "policy: unknown"
	}
	var bits []string
	if c.IssueCreatedAt != "" {
		bits = append(bits, "opened "+text.Clip(c.IssueCreatedAt, 7))
	}
	bits = append(bits, commas(f.Stars)+"★", orElse(f.PrimaryLanguage, "?"))
	if f.RequiresDCO {
		bits = append(bits, "DCO")
	} else {
		bits = append(bits, "no DCO")
	}
	if f.RequiresCLA {
		bits = append(bits, "**CLA required**")
	}
	switch {
	case f.BansAIPRs:
		bits = append(bits, "**BANS AI PRs**")
	case f.RequiresAIDisclosure:
		bits = append(bits, "AI disclosure required (goes in PR body only)")
	}
	if !f.HasTests {
		bits = append(bits, "no tests")
	}
	return strings.Join(bits, " · ")
}

func why(c *model.Candidate) string {
	if c.RejectReason != "" {
		return c.RejectReason
	}
	if n := len(c.ScoreFailures); n > 0 {
		if n > 3 {
			return strings.Join(c.ScoreFailures[:3], "; ")
		}
		return strings.Join(c.ScoreFailures, "; ")
	}
	return "no reason recorded"
}

type group struct {
	label   string
	entries []Entry
}

// groups preserves the ranked order and emits a heading each time the domain
// changes, rather than sorting into buckets: re-sorting would undo the ranking
// the page depends on.
func groups(entries []Entry) []group {
	var out []group
	for _, e := range entries {
		label := "Unmatched by any domain"
		if e.HasDomain {
			label = fmt.Sprintf("%s (weight %d)", orElse(e.Domain.Label, e.Domain.ID), weight(e))
		}
		if len(out) > 0 && out[len(out)-1].label == label {
			out[len(out)-1].entries = append(out[len(out)-1].entries, e)
			continue
		}
		out = append(out, group{label: label, entries: []Entry{e}})
	}
	return out
}

func orElse(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func commas(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return s
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}
