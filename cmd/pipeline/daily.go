package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/halt"
	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/store"
)

// dailyCmd is one full cycle, in dependency order. It is what launchd runs.
//
// Open pull requests are serviced first, because an unanswered review on work
// that already exists costs more than a new candidate found a day late.
//
// Every stage is isolated: one failing must not take the others down. A sweep
// that dies because GitHub returned a 502 to the watcher is a sweep that found
// nothing all day, and the failure that caused it is usually transient.
//
// No run lock is taken here. launchd will not start a second instance of the
// same label, so the collision this would guard against is a manual run
// overlapping the scheduled one -- and the only stage where that is dangerous,
// implement, takes the lock itself.
func dailyCmd(root string, args []string) int {
	execute := false
	skipImplement := false
	for _, a := range args {
		switch a {
		case "--execute":
			execute = true
		case "--no-implement":
			skipImplement = true
		default:
			fmt.Fprintf(os.Stderr, "unknown argument %q\n", a)
			return 2
		}
	}
	if reason, active := halt.New(root).Active(); active {
		fmt.Fprintf(os.Stderr, "HALT is set (%s); nothing will run\n", reason)
		return 1
	}
	pass := []string{}
	if execute {
		pass = append(pass, "--execute")
	}

	failed := 0
	stage := func(name string, fn func() int) {
		fmt.Printf("\n=== %s ===\n", name)
		if code := fn(); code != 0 {
			fmt.Fprintf(os.Stderr, "STAGE FAILED: %s (exit %d)\n", name, code)
			failed++
		}
	}

	stage("1. watch open pull requests", func() int { return watchCmd(root, pass) })
	if skipImplement {
		fmt.Println("\n=== 2. implement approved === (skipped)")
	} else {
		stage("2. implement approved", func() int { return implementApproved(root, execute) })
	}
	stage("3. discover", func() int { return discoverCmd(root, pass) })
	stage("4. triage", func() int { return triageCmd(root, pass) })
	stage("5. propose", func() int { return proposeCmd(root) })
	stage("6. ledger", func() int { return ledgerCmd(root) })

	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d stage(s) failed\n", failed)
		return 1
	}
	return 0
}

// implementApproved builds and submits the human-approved queue, oldest
// approval first, until the caps say stop.
//
// The caps are consulted here as well as inside implement, so that a day's
// budget being spent stops the loop instead of producing one refusal per
// remaining candidate -- and so the reason is said once.
func implementApproved(root string, execute bool) int {
	cfg, err := policy.Load(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	st := store.New(root)
	queue := st.ByStatus(model.StatusApproved, model.StatusAutoApproved)
	if len(queue) == 0 {
		fmt.Println("nothing approved")
		return 0
	}
	// Oldest approval first. A candidate that has waited is a candidate whose
	// issue is getting staler, and the newest approval is the one most likely
	// to be reconsidered.
	sortByApprovedAt(queue)

	problems := 0
	for _, c := range queue {
		if len(c.Blockers) > 0 {
			fmt.Printf("%s: blocked -- %s\n", c.Slug(), c.Blockers[0])
			continue
		}
		all, _ := st.All()
		if held := cfg.Policy.Caps.Check(all, c, time.Now()); len(held) > 0 {
			fmt.Printf("%s: held (%s)\n", c.Slug(), strings.Join(held, "; "))
			// Not a failure, and not a reason to try the next one either: the
			// open and per-day caps that stopped this will stop that too.
			if heldByADailyCap(held) {
				break
			}
			continue
		}
		fmt.Printf("\n--- %s: %s#%d ---\n", c.Slug(), c.Repo, c.Issue)
		args := []string{c.Slug()}
		if execute {
			args = append(args, "--execute")
		}
		if code := implementCmd(root, args); code != 0 {
			problems++
		}
	}
	if problems > 0 {
		return 1
	}
	return 0
}

// heldByADailyCap distinguishes a cap that the next candidate would also hit
// from one that is specific to this repository or organisation.
func heldByADailyCap(reasons []string) bool {
	for _, r := range reasons {
		if strings.Contains(r, "opened today") || strings.Contains(r, "already open (cap") {
			return true
		}
	}
	return false
}

func sortByApprovedAt(cs []*model.Candidate) {
	key := func(c *model.Candidate) string {
		for i := len(c.History) - 1; i >= 0; i-- {
			h := c.History[i]
			if h.To == string(model.StatusApproved) || h.To == string(model.StatusAutoApproved) {
				return h.At
			}
		}
		return ""
	}
	// Insertion sort: the queue is single digits, and this keeps the
	// comparison function readable next to the key it sorts on.
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && key(cs[j]) < key(cs[j-1]); j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}
