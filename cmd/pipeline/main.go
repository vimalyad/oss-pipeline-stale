// Command pipeline is the single binary for the OSS contribution pipeline.
//
// Stages run as one-shots under launchd rather than inside a long-lived
// supervisor: a wedged subprocess stays bounded by process exit, and launchd
// re-runs jobs missed while the laptop was asleep.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vimalyad/osspipeline/internal/guard"
	"github.com/vimalyad/osspipeline/internal/halt"
	"github.com/vimalyad/osspipeline/internal/identity"
	"github.com/vimalyad/osspipeline/internal/machine"
	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/score"
	"github.com/vimalyad/osspipeline/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	root, err := findRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	var code int
	switch os.Args[1] {
	case "doctor":
		code = doctor(root)
	case "machine":
		code = machineCmd(root)
	case "status":
		code = status(root)
	case "rescore":
		code = rescore(root)
	case "check":
		code = checkText()
	case "halt":
		code = engageHalt(root)
	case "resume":
		code = release(root)
	case "ledger":
		code = ledgerCmd(root)
	case "propose":
		code = proposeCmd(root)
	case "approve":
		code = approveCmd(root, os.Args[2:])
	case "reject":
		code = rejectCmd(root, os.Args[2:])
	case "exclude":
		code = excludeCmd(root, os.Args[2:])
	case "cla-signed":
		code = claSignedCmd(root, os.Args[2:])
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `pipeline <command>

  doctor    check stored state, identity config and the kill switch
  machine   detect what this computer can verify, and re-check every candidate
  status    what is tracked and what is waiting on you
  rescore   re-run the scorer over stored candidates (offline, no API calls)
  check     screen a file destined for a public comment or PR body
  halt      stop every scheduled stage (takes a reason)
  resume    lift a halt

  propose   rank what is waiting and write today's report
  approve   <slug>            accept a proposal for implementation
  reject    <slug> <reason>   decline one, with a reason that is kept
  ledger    write the contribution record
  exclude   <owner/repo>      take a repository off the table
  cla-signed <owner/repo>     record that its CLA is signed
`)
}

// findRoot locates the pipeline root by walking up for config/identity.env.
// Deliberately not relative to the executable: the binary may be installed
// anywhere, while the state lives where the user keeps it.
func findRoot() (string, error) {
	if r := os.Getenv("OSSP_ROOT"); r != "" {
		return r, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "config", "identity.env")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no config/identity.env found above the working directory; " +
				"run from the pipeline root or set OSSP_ROOT")
		}
		dir = parent
	}
}

// doctor reports on everything that can be checked without touching the
// network. It exits non-zero if anything is actually wrong, so it can be
// wired into a scheduled run.
func doctor(root string) int {
	problems := 0
	fmt.Printf("root: %s\n\n", root)

	// --- identity -------------------------------------------------------
	id, err := identity.Load(filepath.Join(root, "config", "identity.env"))
	if err != nil {
		fmt.Printf("identity        FAIL  %v\n", err)
		problems++
	} else {
		fmt.Printf("identity        ok    %s <%s>\n", id.Name, id.Email)
		for _, p := range []struct{ what, path string }{
			{"credential helper", id.HelperPath()},
			{"git hooks", id.HooksPath()},
		} {
			if _, err := os.Stat(p.path); err != nil {
				fmt.Printf("  %-14s FAIL  missing: %s\n", p.what, p.path)
				problems++
			}
		}
	}

	// --- kill switch ----------------------------------------------------
	if reason, halted := halt.New(root).Active(); halted {
		fmt.Printf("halt            HALTED  %s\n", firstLine(reason))
	} else {
		fmt.Printf("halt            ok    running\n")
	}

	// --- stored state ---------------------------------------------------
	s := store.New(root)
	cands, bad := s.All()
	fmt.Printf("candidates      %-5s %d loaded, %d unreadable\n",
		okIf(len(bad) == 0), len(cands), len(bad))
	for _, b := range bad {
		fmt.Printf("  FAIL  %s: %v\n", b.Slug, b.Err)
		problems++
	}

	// Validate every recorded history edge against the state machine. v1
	// allowed hand-edited JSON, and three files acquired edges that the
	// table forbids -- invisible until something downstream misbehaved.
	illegal := 0
	for _, c := range cands {
		for i, h := range c.History {
			if h.Forced {
				continue // a sanctioned bypass, recorded as such
			}
			from, to := model.Status(h.From), model.Status(h.To)
			if from == "" || to == "" {
				continue
			}
			if !model.Transitions[from][to] {
				if _, ok := model.ReopenEdges[[2]model.Status{from, to}]; ok {
					fmt.Printf("  WARN  %s history[%d]: %s -> %s is a reopen edge "+
						"but is not marked forced (hand-edited?)\n", c.Slug(), i, from, to)
					continue
				}
				fmt.Printf("  FAIL  %s history[%d]: %s -> %s is not a legal edge\n",
					c.Slug(), i, from, to)
				illegal++
			}
		}
	}
	fmt.Printf("history         %-5s %d illegal edge(s)\n", okIf(illegal == 0), illegal)
	problems += illegal

	byStatus := map[model.Status]int{}
	for _, c := range cands {
		byStatus[c.Status]++
	}
	fmt.Printf("\nby status:\n")
	for _, st := range []model.Status{
		model.StatusProposed, model.StatusApproved, model.StatusImplementing,
		model.StatusPushed, model.StatusPROpen, model.StatusChangesRequested,
		model.StatusUpdating, model.StatusMerged, model.StatusClosed,
		model.StatusRejected, model.StatusAbandoned, model.StatusStale,
	} {
		if n := byStatus[st]; n > 0 {
			fmt.Printf("  %-18s %d\n", st, n)
		}
	}

	fmt.Printf("\n%d problem(s)\n", problems)
	if problems > 0 {
		return 1
	}
	return 0
}

func engageHalt(root string) int {
	reason := "halted from the command line"
	if len(os.Args) > 2 {
		reason = os.Args[2]
	}
	if err := halt.New(root).Engage(reason, "cli"); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("halted:", reason)
	fmt.Println("scheduled stages will now exit immediately. `pipeline resume` to lift.")
	return 0
}

func release(root string) int {
	if err := halt.New(root).Release(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("resumed")
	return 0
}

func okIf(b bool) string {
	if b {
		return "ok"
	}
	return "FAIL"
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}

// machineCmd detects this machine's capabilities and then re-checks every
// stored candidate against them, so the effect of the gate is visible on real
// data rather than asserted.
func machineCmd(root string) int {
	ctx := context.Background()
	p := machine.Detect(ctx)
	if err := machine.Save(root, p); err != nil {
		fmt.Fprintln(os.Stderr, "warn: could not cache profile:", err)
	}
	fmt.Print(p.Summary())
	fmt.Printf("capabilities: %v\n\n", p.Capabilities)

	cands, _ := store.New(root).All()
	type row struct {
		slug, lane, reason string
		reject, host       bool
	}
	var rows []row
	counts := map[string]int{}
	for _, c := range cands {
		var texts []string
		texts = append(texts, c.Title)
		for _, l := range c.Labels {
			texts = append(texts, l)
		}
		if c.Brief != nil {
			texts = append(texts, c.Brief.Reproduction,
				c.Brief.MaintainerDesiredApproach,
				strings.Join(c.Brief.AcceptanceCriteria, " "))
		}
		d := machine.Decide(p, machine.Infer(texts...))
		key := "container"
		switch {
		case d.Reject:
			key = "REJECT"
		case d.HostOnly:
			key = "host-only"
		case d.Lane == machine.LaneNone:
			key = "blocked"
		}
		counts[key]++
		if d.Reject || d.HostOnly {
			rows = append(rows, row{c.Slug(), key, d.Reason, d.Reject, d.HostOnly})
		}
	}

	fmt.Printf("%d candidates re-checked against this machine:\n", len(cands))
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-10s %d\n", k, counts[k])
	}
	if len(rows) > 0 {
		fmt.Printf("\nwould be routed or refused:\n")
		sort.Slice(rows, func(i, j int) bool { return rows[i].slug < rows[j].slug })
		for _, r := range rows {
			fmt.Printf("  [%s] %s\n      %s\n", r.lane, r.slug, r.reason)
		}
	}
	return 0
}

// status is the at-a-glance view. Its output is deliberately identical to the
// Python implementation's, so the two can be diffed during the port -- that
// comparison is the safety net for the whole rewrite.
func status(root string) int {
	cands, bad := store.New(root).All()
	counts := map[model.Status]int{}
	for _, c := range cands {
		counts[c.Status]++
	}
	fmt.Printf("%d candidates tracked\n", len(cands))

	type kv struct {
		s model.Status
		n int
	}
	rows := make([]kv, 0, len(counts))
	for s, n := range counts {
		rows = append(rows, kv{s, n})
	}
	// Most common first; ties broken by name so runs are reproducible.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].s < rows[j].s
	})
	for _, r := range rows {
		fmt.Printf("  %4d  %s\n", r.n, r.s)
	}

	live := map[model.Status]bool{
		model.StatusProposed: true, model.StatusApproved: true,
		model.StatusPushed: true, model.StatusPROpen: true,
		model.StatusChangesRequested: true, model.StatusUpdating: true,
	}
	var waiting []*model.Candidate
	for _, c := range cands {
		if live[c.Status] {
			waiting = append(waiting, c)
		}
	}
	if len(waiting) > 0 {
		fmt.Printf("\nawaiting action:\n")
		for _, c := range waiting {
			fmt.Printf("  %-20s %s#%d  %s\n", c.Status, c.Repo, c.Issue, truncate(c.Title, 46))
		}
	}
	for _, b := range bad {
		fmt.Fprintf(os.Stderr, "  warn: skipping unreadable %s: %v\n", b.Slug, b.Err)
	}
	return 0
}

// truncate cuts to n runes, matching Python's slice semantics on the titles
// we actually store (which are not ASCII-only).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// rescore re-runs the scorer over every stored candidate using only what is
// already on disk: no API calls, no LLM calls, no network. That makes it free
// to run, which makes it the parity gate for the port -- the same 224 inputs
// must produce the same verdicts in both implementations.
//
// Output is one line per candidate, sorted, designed to be diffed.
func rescore(root string) int {
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
	st := store.New(root)
	cands, _ := st.All()
	sort.Slice(cands, func(i, j int) bool { return cands[i].Slug() < cands[j].Slug() })

	deps := score.Deps{
		MissingToolchain: missingToolchain,
		LoadFacts: func(repo string) *model.RepoFacts {
			f, err := st.LoadRepoFacts(repo)
			if err != nil {
				return nil
			}
			return f
		},
	}
	pass := 0
	for _, c := range cands {
		r := score.Score(c, cfg, id.OtherLogins, deps)
		verdict := "REJECT"
		if r.OK {
			verdict = "PASS"
			pass++
		}
		fmt.Printf("%s\t%s\t%s\n", c.Slug(), verdict, strings.Join(r.Fails, " | "))
	}
	fmt.Fprintf(os.Stderr, "\n%d/%d pass\n", pass, len(cands))
	return 0
}

// languageBinaries maps a language to the binary needed to build it here.
// Carried over verbatim so Go and Python agree during the port; container
// recipe resolution replaces this entirely once the sandbox lands.
var languageBinaries = map[string]string{
	"go": "go", "rust": "cargo", "python": "uv",
	"typescript": "npm", "javascript": "npm",
	"c++": "cmake", "c": "cmake", "java": "mvn", "ruby": "bundle",
}

func missingToolchain(language string) string {
	bin, ok := languageBinaries[strings.ToLower(strings.TrimSpace(language))]
	if !ok {
		return ""
	}
	if _, err := exec.LookPath(bin); err != nil {
		return bin
	}
	return ""
}

// checkText screens a file that is about to be posted in public.
//
// Everything this pipeline publishes goes through here first. The forbidden
// vocabulary is not a style preference: an implementer's closing notes once
// reached a real pull request, complete with first-person remarks about
// denied commands and a claim that tests had not been run when they had.
func checkText() int {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: pipeline check <file>")
		return 2
	}
	b, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	text := string(b)
	bad := 0
	if phrases := guard.CheckBody(text); len(phrases) > 0 {
		fmt.Printf("FORBIDDEN  %v\n", phrases)
		bad++
	}
	if secrets := guard.ScanSecrets(text); len(secrets) > 0 {
		fmt.Printf("SECRETS    %v\n", secrets)
		bad++
	}
	words := len(strings.Fields(text))
	if bad == 0 {
		fmt.Printf("clean      %d words, %d lines\n", words,
			strings.Count(text, "\n")+1)
		return 0
	}
	return 1
}
