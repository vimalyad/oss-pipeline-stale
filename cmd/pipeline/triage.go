package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/audit"
	"github.com/vimalyad/osspipeline/internal/brief"
	"github.com/vimalyad/osspipeline/internal/contest"
	"github.com/vimalyad/osspipeline/internal/ghx"
	"github.com/vimalyad/osspipeline/internal/halt"
	"github.com/vimalyad/osspipeline/internal/harvest"
	"github.com/vimalyad/osspipeline/internal/identity"
	"github.com/vimalyad/osspipeline/internal/llm"
	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/repofacts"
	"github.com/vimalyad/osspipeline/internal/score"
	"github.com/vimalyad/osspipeline/internal/store"
)

// triageCmd takes what discover found and decides what to put in front of a
// person. It is the middle of the intake, and until now it had no command:
// contest, repofacts, harvest and brief were ported and tested but nothing
// called them, so the Go binary could discover candidates and report on them
// and do nothing in between.
//
// Three phases, cheapest first, because the expensive one costs a model call
// and a full issue thread. Nothing here reaches anybody: the whole command
// writes to disk and stops at `proposed`, where the human gate is.
func triageCmd(root string, args []string) int {
	execute := false
	maxHarvest := 12
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--execute":
			execute = true
		case strings.HasPrefix(a, "--max-harvest="):
			if _, err := fmt.Sscanf(a, "--max-harvest=%d", &maxHarvest); err != nil || maxHarvest < 1 {
				fmt.Fprintf(os.Stderr, "bad --max-harvest %q\n", a)
				return 2
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown argument %q\n", a)
			return 2
		}
	}
	if reason, active := halt.New(root).Active(); active {
		fmt.Fprintf(os.Stderr, "HALT is set (%s); not triaging\n", reason)
		return 1
	}

	cfg, err := policy.Load(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	id, err := identity.Load(filepath.Join(root, "config", "identity.env"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	token, err := identity.Token(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	env := identity.Env(id, token)
	gh := ghx.New(env)
	brain := llm.New(env)
	st := store.New(root)
	log := audit.New(root)

	queue := st.ByStatus(model.StatusDiscovered)
	if len(queue) == 0 {
		fmt.Println("nothing to triage")
		return 0
	}
	fmt.Printf("%d discovered candidate(s)\n", len(queue))

	t := triage{
		root: root, cfg: cfg, gh: gh, brain: brain, st: st, log: log,
		facts: func(ctx context.Context, repo string) (*model.RepoFacts, error) {
			return repofacts.Fetch(ctx, plainGet{gh}, brain, st, repo, repofacts.Options{
				FirstTimeWindow: cfg.Policy.Scoring.FirstTimeWindowDays,
			})
		},
		otherLogins: id.OtherLogins, execute: execute, maxHarvest: maxHarvest,
	}
	return t.run(context.Background(), queue)
}

// The collaborators are the narrow interfaces the phases use, so the phase
// ordering can be tested without a network: the rule that phase B must not
// reject on a bar only a brief can answer is the kind of thing that is true
// when written and quietly stops being true later.
type triage struct {
	root        string
	cfg         *policy.Config
	gh          triageAPI
	facts       factsFetcher
	brain       thread
	st          triageStore
	log         recorder
	otherLogins []string
	execute     bool
	maxHarvest  int
}

type triageAPI interface {
	contest.API
	harvest.API
}

// factsFetcher is repofacts behind a function, because Fetch takes four
// collaborators of its own and the wiring for them belongs at the wiring
// layer, not in the middle of a phase.
type factsFetcher func(ctx context.Context, repo string) (*model.RepoFacts, error)

type thread interface {
	brief.Judge
}

type triageStore interface {
	ByStatus(want ...model.Status) []*model.Candidate
	Save(c *model.Candidate) (string, error)
	LoadRepoFacts(repo string) (*model.RepoFacts, error)
}

// reject records a decision without transitioning when this is a dry run. The
// reason is stored either way, because seeing why a candidate would be dropped
// is most of what a dry run is for.
func (t triage) reject(c *model.Candidate, why string) {
	c.RejectReason = why
	if !t.execute {
		return
	}
	if err := model.Transition(c, model.StatusRejected, why); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", c.Slug(), err)
		return
	}
	if _, err := t.st.Save(c); err != nil {
		fmt.Fprintf(os.Stderr, "save %s: %v\n", c.Slug(), err)
	}
}

func (t triage) save(c *model.Candidate) {
	if !t.execute {
		return
	}
	if _, err := t.st.Save(c); err != nil {
		fmt.Fprintf(os.Stderr, "save %s: %v\n", c.Slug(), err)
	}
}

func (t triage) run(ctx context.Context, queue []*model.Candidate) int {
	now := time.Now()

	// --- phase A: contest -------------------------------------------------
	// First because it is the cheapest thing that can remove a candidate, and
	// because everything after it is wasted on an issue somebody else is
	// already working on.
	fmt.Printf("\nphase A: contest (%d)\n", len(queue))
	var targetable []*model.Candidate
	for _, c := range queue {
		verdict, sig, err := contest.Classify(ctx, t.gh, c.Repo, linkedPRs(c),
			t.cfg.Policy.Staleness, now)
		if err != nil {
			t.reject(c, "contest classification failed: "+oneLine(err.Error()))
			continue
		}
		c.Contest, c.PRSignal = verdict, sig
		if !verdict.Targetable() {
			why := "contest=" + string(verdict)
			if sig != nil && len(sig.Reasons) > 0 {
				why += " (" + sig.Reasons[0] + ")"
			}
			t.reject(c, why)
			continue
		}
		targetable = append(targetable, c)
	}
	fmt.Printf("  %d targetable, %d contested or claimed\n",
		len(targetable), len(queue)-len(targetable))

	// --- phase B: the bars that do not need a thread ----------------------
	// Repo facts are cached weekly, so this is usually free. Scoring here
	// drops a candidate before it costs a full issue thread and a model call;
	// the bars that need a brief are deliberately ignored at this point,
	// because the brief does not exist yet.
	fmt.Printf("\nphase B: repo facts and the cheap bars (%d)\n", len(targetable))
	var survivors []*model.Candidate
	for _, c := range targetable {
		f, err := t.facts(ctx, c.Repo)
		if err != nil {
			t.reject(c, "repo facts unavailable: "+oneLine(err.Error()))
			continue
		}
		c.Facts = f
		r := t.score(c, now)
		if blocking := withoutThreadBars(r.Fails); len(blocking) > 0 {
			c.ScoreFailures = blocking
			t.reject(c, strings.Join(first(blocking, 2), "; "))
			continue
		}
		survivors = append(survivors, c)
	}
	fmt.Printf("  %d survived\n", len(survivors))

	// --- phase C: harvest and brief ---------------------------------------
	// Capped, because this is the phase that costs money and rate limit. The
	// overflow is rejected with a reason that says so, and store.ShouldReconsider
	// treats that as transient, so a deferred candidate comes back rather than
	// being lost.
	// Spread across repositories before the cap bites. Discovery interleaves
	// for exactly this reason and the effect is undone here otherwise: the
	// first run of this command spent its whole budget on four argo-cd issues,
	// of which the org cooldown would have let at most one become a pull
	// request. The expensive phase has to sample the breadth, not the
	// alphabet.
	survivors = interleaveByRepo(survivors)
	n := min(len(survivors), t.maxHarvest)
	fmt.Printf("\nphase C: harvest and brief (%d of %d)\n", n, len(survivors))
	proposed := 0
	for _, c := range survivors[:n] {
		if !t.harvestAndBrief(ctx, c) {
			continue
		}
		r := t.score(c, now)
		c.ScoreFailures, c.SoftPenalties, c.Blockers = r.Fails, r.Penalties, r.Blockers
		if !r.OK {
			t.reject(c, strings.Join(first(r.Fails, 2), "; "))
			fmt.Printf("  reject   %s#%d  %s\n", c.Repo, c.Issue, first1(r.Fails, 60))
			continue
		}
		if t.execute {
			if err := model.Transition(c, model.StatusScored, "cleared all bars"); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", c.Slug(), err)
				continue
			}
			if err := model.Transition(c, model.StatusProposed, "awaiting human approval"); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", c.Slug(), err)
				continue
			}
		}
		t.save(c)
		proposed++
		fmt.Printf("  PROPOSE  %s#%d  %s\n", c.Repo, c.Issue, clip(c.Title, 50))
	}
	for _, c := range survivors[n:] {
		t.reject(c, fmt.Sprintf("deferred: exceeded max_harvest=%d this run", t.maxHarvest))
	}

	fmt.Printf("\n%d proposed\n", proposed)
	if !t.execute {
		fmt.Println("[dry run] nothing was written; run with --execute to record these decisions")
		return 0
	}
	_ = t.log.Record("triage", "", fmt.Sprintf("%d in, %d proposed", len(queue), proposed))
	return 0
}

// harvestAndBrief fetches the thread and distils it. Reports whether the
// candidate is still in play.
func (t triage) harvestAndBrief(ctx context.Context, c *model.Candidate) bool {
	p, err := harvest.Harvest(ctx, t.gh, c.Repo, c.Issue)
	if err != nil {
		t.reject(c, "harvest failed: "+oneLine(err.Error()))
		return false
	}
	if t.execute {
		if _, err := harvest.Save(t.root, c.Slug(), p); err != nil {
			fmt.Fprintf(os.Stderr, "saving the thread for %s: %v\n", c.Slug(), err)
		}
	}
	// Two renderings: one bounded for the prompt, one whole for verifying the
	// quotes. A quote may legitimately come from the part the cap trimmed.
	res, err := brief.Extract(ctx, t.brain, harvest.Render(p, 0), harvest.Render(p, maxInt))
	if err != nil {
		t.reject(c, "brief failed: "+oneLine(err.Error()))
		return false
	}
	c.Brief = &res.Brief
	for _, d := range res.Dropped {
		_ = t.log.Record("brief_dropped", c.Slug(), d)
	}
	return true
}

func (t triage) score(c *model.Candidate, now time.Time) score.Result {
	return score.Score(c, t.cfg, t.otherLogins, score.Deps{
		MissingToolchain: missingToolchain,
		LoadFacts: func(repo string) *model.RepoFacts {
			f, err := t.st.LoadRepoFacts(repo)
			if err != nil {
				return nil
			}
			return f
		},
		Now: func() time.Time { return now },
	})
}

const maxInt = int(^uint(0) >> 1)

// linkedPRs rebuilds what discover saw. The URL is derivable, so only the
// numbers are stored.
func linkedPRs(c *model.Candidate) []contest.LinkedPR {
	out := make([]contest.LinkedPR, 0, len(c.LinkedPRs))
	for _, n := range c.LinkedPRs {
		out = append(out, contest.LinkedPR{
			Number: n,
			URL:    fmt.Sprintf("https://github.com/%s/pull/%d", c.Repo, n),
		})
	}
	return out
}

// withoutThreadBars drops the score failures that only a brief can answer.
//
// Phase B runs before the thread has been fetched, so those bars are not
// failures yet -- they are questions nothing has asked. Rejecting on them here
// would reject every candidate for the crime of not having been harvested.
func withoutThreadBars(fails []string) []string {
	var out []string
	for _, f := range fails {
		l := strings.ToLower(f)
		if strings.Contains(l, "brief") || strings.Contains(l, "maintainer") ||
			strings.Contains(l, "converged") || strings.Contains(l, "claimed by") {
			continue
		}
		out = append(out, f)
	}
	return out
}

func first(s []string, n int) []string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func first1(s []string, n int) string {
	if len(s) == 0 {
		return ""
	}
	return clip(s[0], n)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// interleaveByRepo reorders so consecutive entries come from different
// repositories: one from each, then a second from each, and so on. Order
// within a repository is preserved, so the ranking discovery gave survives.
func interleaveByRepo(cs []*model.Candidate) []*model.Candidate {
	var order []string
	buckets := map[string][]*model.Candidate{}
	for _, c := range cs {
		if _, ok := buckets[c.Repo]; !ok {
			order = append(order, c.Repo)
		}
		buckets[c.Repo] = append(buckets[c.Repo], c)
	}
	out := make([]*model.Candidate, 0, len(cs))
	for depth := 0; len(out) < len(cs); depth++ {
		for _, repo := range order {
			if b := buckets[repo]; depth < len(b) {
				out = append(out, b[depth])
			}
		}
	}
	return out
}
