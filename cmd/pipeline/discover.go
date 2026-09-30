package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vimalyad/osspipeline/internal/audit"
	"github.com/vimalyad/osspipeline/internal/discover"
	"github.com/vimalyad/osspipeline/internal/ghx"
	"github.com/vimalyad/osspipeline/internal/halt"
	"github.com/vimalyad/osspipeline/internal/identity"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/profile"
	"github.com/vimalyad/osspipeline/internal/sources"
	"github.com/vimalyad/osspipeline/internal/store"
)

// discoverCmd sweeps the watchlist for candidate issues.
//
// Read-only unless --execute. A dry run says what it found and writes nothing,
// which is what makes it safe to run while wondering whether the sweep is
// working at all.
func discoverCmd(root string, args []string) int {
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
		fmt.Fprintf(os.Stderr, "HALT is set (%s); not discovering\n", reason)
		return 1
	}

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
	gh := ghx.New(identity.Env(id, token))
	st := store.New(root)

	// The sweep order comes from the domain weights, so a weight-3 domain gets
	// three times the share of the pass. Excluded repositories are dropped
	// here rather than after the search, which is the difference between not
	// asking and asking and throwing the answer away.
	cursor := loadCursor(root)
	slots, next := sources.Allocate(prof.Domains, cfg.Policy.Caps.MaxCandidatesPerRun*2, cursor)
	seen := map[string]bool{}
	var repos []string
	for _, s := range slots {
		if seen[s.Repo] || cfg.Excluded(s.Repo) {
			continue
		}
		seen[s.Repo] = true
		repos = append(repos, s.Repo)
	}
	if len(repos) == 0 {
		fmt.Println("no repositories to sweep")
		return 0
	}

	res := discover.Sweep(context.Background(), gh, st, st, repos, cfg.Policy.AcceptanceLabels,
		discover.Options{
			PerRepo:             10,
			Cap:                 cfg.Policy.Caps.MaxCandidatesPerRun,
			ReconsiderAfterDays: cfg.Policy.Caps.ReconsiderAfterDays,
			Concurrency:         6,
			Now:                 time.Now,
			Log:                 func(s string) { fmt.Println(s) },
		})

	if !execute {
		fmt.Println("\n[dry run] nothing was written; run with --execute to record these candidates")
		return 0
	}
	log := audit.New(root)
	written := 0
	for _, f := range res.Found {
		c := f.Candidate
		linked := make([]int, 0, len(f.LinkedPRs))
		for _, pr := range f.LinkedPRs {
			linked = append(linked, pr.Number)
		}
		// A candidate already on disk keeps its history: overwriting it would
		// discard a rejection reason, a brief, or a merged pull request.
		if existing, err := st.Load(c.Slug()); err == nil {
			existing.Title, existing.Labels = c.Title, c.Labels
			existing.Comments, existing.Reactions = c.Comments, c.Reactions
			existing.IssueUpdatedAt = c.IssueUpdatedAt
			c = existing
		}
		// The search just told us what the timeline points at, so this is
		// fresher than whatever a previous sweep recorded.
		c.LinkedPRs = linked
		if _, err := st.Save(c); err != nil {
			fmt.Fprintf(os.Stderr, "save %s: %v\n", c.Slug(), err)
			continue
		}
		written++
	}
	saveCursor(root, next)
	_ = log.Record("discover", "", fmt.Sprintf("%d candidates across %d repos, %d skipped",
		len(res.Found), len(repos), len(res.Skipped)))
	fmt.Printf("\nwrote %d candidate(s)\n", written)
	if len(res.Skipped) > 0 {
		return 1 // a silent partial sweep is how a broken token goes unnoticed
	}
	return 0
}

// The cursor is what makes the next pass continue rather than restart. Stored
// beside the candidates because it is state, not configuration.
func cursorPath(root string) string {
	return filepath.Join(root, "state", "discover-cursor.json")
}

func loadCursor(root string) sources.Cursor {
	c := sources.Cursor{}
	b, err := os.ReadFile(cursorPath(root))
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

func saveCursor(root string, c sources.Cursor) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(cursorPath(root)), 0o755)
	_ = os.WriteFile(cursorPath(root), b, 0o644)
}
