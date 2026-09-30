package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vimalyad/osspipeline/internal/audit"
	"github.com/vimalyad/osspipeline/internal/cilog"
	"github.com/vimalyad/osspipeline/internal/ghx"
	"github.com/vimalyad/osspipeline/internal/halt"
	"github.com/vimalyad/osspipeline/internal/identity"
	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/notify"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/profile"
	"github.com/vimalyad/osspipeline/internal/store"
	"github.com/vimalyad/osspipeline/internal/watch"
)

// watchCmd runs one cycle over every open pull request.
//
// Read-only unless --execute. Without it nothing is marked seen, so a dry run
// can be repeated and the next real cycle still finds everything -- the
// alternative is a dry run that quietly consumes the feedback it was only
// supposed to look at.
func watchCmd(root string, args []string) int {
	execute := false
	for _, a := range args {
		if a == "--execute" {
			execute = true
		} else {
			fmt.Fprintf(os.Stderr, "unknown argument %q\n", a)
			return 2
		}
	}

	if reason, active := halt.New(root).Active(); active {
		fmt.Fprintf(os.Stderr, "HALT is set (%s); not watching\n", reason)
		return 1
	}

	ctx := context.Background()
	st := store.New(root)
	open := st.ByStatus(model.OpenStatuses...)
	if len(open) == 0 {
		fmt.Println("no open pull requests")
		return 0
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
	cfg, err := policy.Load(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	prof, err := profile.Load(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	note := notifier(root, prof)

	log := audit.New(root)
	problems := 0
	for _, c := range open {
		d := watch.Deps{
			API:    gh,
			Checks: &cilog.Fetcher{API: cilogAPI{gh}, Repo: c.Repo},
			Store:  st,
			Audit:  func(kind, slug, detail string) { _ = log.Record(kind, slug, detail) },
			Now:    time.Now,
			// A dry run must not push to the phone. Everything else about a
			// dry run is "look but do not touch", and a notification is a
			// side effect the user cannot undo by re-running.
			Notify: func(e notify.Event) {
				if !execute {
					fmt.Printf("    [dry run] would notify p%d: %s\n", e.Priority, e.Title)
					return
				}
				sent, err := note.Send(ctx, e)
				if err != nil {
					fmt.Fprintln(os.Stderr, "    notify:", err)
				} else if sent {
					fmt.Printf("    notified: %s\n", e.Title)
				}
			},
			// No Fixer and no Feedback classifier yet: without them a cycle
			// reports and queues but never pushes, which is the correct
			// behaviour to ship first.
			StaleAfterDays: cfg.Policy.Staleness.PRUntouchedDays,
		}
		out, err := watch.Sync(ctx, d, c, execute)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%-34s ERROR %v\n", c.Slug(), err)
			problems++
			continue
		}
		fmt.Printf("%-34s %-18s %s\n", c.Slug(), out.Status, out.Summary)
		for _, chk := range out.State.Failing {
			fmt.Printf("    FAILING %s\n", chk.Name)
		}
		for _, it := range out.State.Queued() {
			fmt.Printf("    needs a human [%s] %s: %s\n", it.Class, it.Author, firstN(it.Why, 60))
		}
	}
	if !execute {
		fmt.Println("\n[dry run] nothing was marked seen; run with --execute to record this cycle")
	}
	if problems > 0 {
		return 1
	}
	return 0
}
