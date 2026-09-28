package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/audit"
	"github.com/vimalyad/osspipeline/internal/gate"
	"github.com/vimalyad/osspipeline/internal/ledger"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/profile"
	"github.com/vimalyad/osspipeline/internal/propose"
	"github.com/vimalyad/osspipeline/internal/store"
)

// ledgerCmd writes the contribution record.
//
// Reads nothing from the network: every fact it needs is already on disk,
// which is what lets it run when GitHub is down and still be trusted.
func ledgerCmd(root string) int {
	cfg, err := policy.Load(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	cands, bad := store.New(root).All()
	for _, b := range bad {
		fmt.Fprintf(os.Stderr, "unreadable: %s: %v\n", b.Slug, b.Err)
	}
	l := ledger.Compute(cands, ledger.Unlock{
		MinPRs:    cfg.Policy.Watchlist.UnlockMinPRs,
		MergeRate: cfg.Policy.Watchlist.UnlockMergeRate,
	}, time.Now())

	out, err := ledger.Write(root, l)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Print(l.Markdown())
	fmt.Printf("\nwritten to %s\n", out)
	// A disagreement between a record and its own history means a decided pull
	// request is missing from these counts. Exit non-zero so a scheduled run
	// surfaces it rather than printing it into a log nobody reads.
	if len(l.Inconsistent) > 0 {
		return 1
	}
	return 0
}

// proposeCmd writes today's proposal report.
func proposeCmd(root string) int {
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
	cands, bad := store.New(root).All()
	for _, b := range bad {
		fmt.Fprintf(os.Stderr, "unreadable: %s: %v\n", b.Slug, b.Err)
	}

	day := time.Now()
	body := propose.Render(cands, prof, cfg.Policy.Caps, day)
	dir := filepath.Join(root, "reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	path := filepath.Join(dir, day.Format("2006-01-02")+".md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	proposed, rejected, ops := propose.Classify(cands, prof)
	fmt.Printf("%d proposed · %d rejected · %d comment opportunities\n",
		len(proposed), len(rejected), len(ops))
	for _, e := range proposed {
		domain := "unmatched"
		if e.HasDomain {
			domain = e.Domain.ID
		}
		fmt.Printf("  %-34s %-12s %s\n", e.Candidate.Slug(), domain,
			firstN(e.Candidate.Title, 48))
	}
	fmt.Printf("\nwritten to %s\n", path)
	return 0
}

// approveCmd and rejectCmd are the human gate. They take a slug because that
// is what the report prints and what a notification carries.
func approveCmd(root string, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: pipeline approve <slug>")
		return 2
	}
	msg, err := gate.Approve(store.New(root), audit.New(root), args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(msg)
	return 0
}

func rejectCmd(root string, args []string) int {
	// The reason is required by gate, and saying so here is cheaper than
	// letting the error explain it after the fact.
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, `usage: pipeline reject <slug> <reason>

A reason is required: a rejection without one cannot be told apart from one the
pipeline made itself, and a human rejection is never reconsidered.`)
		return 2
	}
	msg, err := gate.Reject(store.New(root), audit.New(root), args[0], strings.Join(args[1:], " "))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(msg)
	return 0
}

func excludeCmd(root string, args []string) int {
	if len(args) != 1 || !strings.Contains(args[0], "/") {
		fmt.Fprintln(os.Stderr, "usage: pipeline exclude <owner/repo>")
		return 2
	}
	ex := gate.Exclusions{Path: filepath.Join(root, "config", "exclusions.yaml")}
	msg, err := gate.Exclude(store.New(root), audit.New(root), ex, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(msg)
	return 0
}

func claSignedCmd(root string, args []string) int {
	if len(args) != 1 || !strings.Contains(args[0], "/") {
		fmt.Fprintln(os.Stderr, "usage: pipeline cla-signed <owner/repo>")
		return 2
	}
	ex := gate.Exclusions{Path: filepath.Join(root, "config", "exclusions.yaml")}
	msg, err := gate.CLASigned(store.New(root), audit.New(root), ex, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(msg)
	return 0
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
