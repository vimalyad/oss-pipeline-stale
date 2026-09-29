package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vimalyad/osspipeline/internal/audit"
	"github.com/vimalyad/osspipeline/internal/ghx"
	"github.com/vimalyad/osspipeline/internal/identity"
	"github.com/vimalyad/osspipeline/internal/ledger"
	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/publish"
	"github.com/vimalyad/osspipeline/internal/report"
	"github.com/vimalyad/osspipeline/internal/store"
	"github.com/vimalyad/osspipeline/internal/text"
	"github.com/vimalyad/osspipeline/internal/watch"
)

// reportCmd writes the single status page, and optionally mirrors it to the
// secret gist that makes it readable from a phone.
func reportCmd(root string, args []string) int {
	doPublish := false
	for _, a := range args {
		if a == "--publish" {
			doPublish = true
		} else {
			fmt.Fprintf(os.Stderr, "unknown argument %q\n", a)
			return 2
		}
	}

	cfg, err := policy.Load(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	st := store.New(root)
	all, bad := st.All()
	for _, b := range bad {
		fmt.Fprintf(os.Stderr, "unreadable: %s: %v\n", b.Slug, b.Err)
	}

	id, err := identity.Load(filepath.Join(root, "config", "identity.env"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	in := report.Input{
		Generated: time.Now(),
		Login:     id.Login,
		Ledger: ledger.Compute(all, ledger.Unlock{
			MinPRs:    cfg.Policy.Watchlist.UnlockMinPRs,
			MergeRate: cfg.Policy.Watchlist.UnlockMergeRate,
		}, time.Now()),
	}

	// Open pull requests need GitHub; everything else is already on disk. The
	// token is only fetched when there is something to ask about, so a quiet
	// day produces a page with no network at all.
	if open := st.ByStatus(model.OpenStatuses...); len(open) > 0 {
		token, terr := identity.Token(id)
		if terr != nil {
			fmt.Fprintln(os.Stderr, "error:", terr)
			return 1
		}
		gh := ghx.New(identity.Env(id, token))
		for _, c := range open {
			in.Open = append(in.Open, prView(context.Background(), gh, c))
		}
	}

	for _, c := range st.ByStatus(model.StatusProposed) {
		if len(c.Blockers) > 0 {
			in.NeedsYou = append(in.NeedsYou, report.Item{
				Text: fmt.Sprintf("**%s#%d** — %s", c.Repo, c.Issue, text.Clip(c.Blockers[0], 90))})
			continue
		}
		in.NeedsYou = append(in.NeedsYou, report.Item{
			Text: fmt.Sprintf("**%s#%d** %s — approve or reject", c.Repo, c.Issue, text.Clip(c.Title, 58)),
			Cmd:  "pipeline approve " + c.Slug(),
		})
	}
	for _, c := range st.ByStatus(model.StatusApproved, model.StatusAutoApproved) {
		if len(c.Blockers) > 0 {
			in.NeedsYou = append(in.NeedsYou, report.Item{
				Text: fmt.Sprintf("**%s#%d** — %s", c.Repo, c.Issue, text.Clip(c.Blockers[0], 90))})
			continue
		}
		in.Queued = append(in.Queued, report.Item{
			Text: fmt.Sprintf("**%s#%d** %s", c.Repo, c.Issue, text.Clip(c.Title, 52)),
			Cmd:  "will run on the next scheduled cycle",
		})
	}

	page := report.Render(in)
	out := filepath.Join(root, "reports", "STATUS.md")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := os.WriteFile(out, []byte(page), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Print(page)
	fmt.Printf("\nwritten to %s\n", out)

	if !doPublish {
		return 0
	}
	token, terr := identity.Token(id)
	if terr != nil {
		fmt.Fprintln(os.Stderr, "error:", terr)
		return 1
	}
	res, perr := publish.Publish(context.Background(), ghx.New(identity.Env(id, token)),
		publish.Store{Root: root}, page)
	switch {
	case errors.Is(perr, publish.ErrScopeMissing):
		// Actionable, not fatal: the page is written either way, and telling
		// the user to check their network would send them to the wrong place.
		fmt.Fprintf(os.Stderr, "\nnot published: the token lacks the `gist` scope.\n"+
			"Add it at https://github.com/settings/tokens and re-run.\n")
		return 1
	case errors.Is(perr, publish.ErrUnreachable):
		fmt.Fprintf(os.Stderr, "\nnot published: could not reach GitHub (%v)\n", perr)
		return 1
	case perr != nil:
		fmt.Fprintln(os.Stderr, "\npublish:", perr)
		return 1
	}
	verb := "updated"
	if res.Created {
		verb = "created"
	}
	fmt.Printf("%s %s\n", verb, res.URL)
	_ = audit.New(root).Record("gist_"+verb, "", res.ID)
	return 0
}

// prView polls one pull request. A failure costs one line rather than the whole
// page: the page is how the user finds out anything at all.
func prView(ctx context.Context, api watch.API, c *model.Candidate) report.PRView {
	v := report.PRView{Repo: c.Repo, Title: c.Title, URL: c.PRURL}
	if c.PRNumber != nil {
		v.Number = *c.PRNumber
	}
	st, err := watch.Poll(ctx, api, c)
	if err != nil {
		v.Err = err.Error()
		return v
	}
	v.URL, v.State, v.Merged = st.URL, st.State, st.Merged
	v.Mergeable = st.Mergeable
	v.CheckCounts = map[string]int{}
	for _, f := range st.Failing {
		v.Failing = append(v.Failing, f.Name)
		v.CheckCounts["failure"]++
	}
	if n := len(st.Awaiting); n > 0 {
		v.CheckCounts["awaiting maintainer approval"] = n
	}
	for _, q := range c.QueuedReplies {
		if posted, _ := q["posted"].(bool); posted {
			continue
		}
		v.PendingReplies++
		if d, _ := q["draft"].(string); d != "" {
			v.DraftedReplies++
		}
	}
	for i := len(st.Items) - 1; i >= 0; i-- {
		if st.Items[i].Author != "CI" {
			v.LastCommentBy = st.Items[i].Author
			break
		}
	}
	return v
}
