package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/audit"
	"github.com/vimalyad/osspipeline/internal/ghx"
	"github.com/vimalyad/osspipeline/internal/guard"
	"github.com/vimalyad/osspipeline/internal/halt"
	"github.com/vimalyad/osspipeline/internal/harvest"
	"github.com/vimalyad/osspipeline/internal/identity"
	"github.com/vimalyad/osspipeline/internal/image"
	"github.com/vimalyad/osspipeline/internal/implement"
	"github.com/vimalyad/osspipeline/internal/llm"
	"github.com/vimalyad/osspipeline/internal/lock"
	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/recipe"
	"github.com/vimalyad/osspipeline/internal/repo"
	"github.com/vimalyad/osspipeline/internal/repro"
	"github.com/vimalyad/osspipeline/internal/sandbox"
	"github.com/vimalyad/osspipeline/internal/store"
	"github.com/vimalyad/osspipeline/internal/submit"
)

// implementCmd runs one candidate all the way to a pull request body.
//
// Dry run is the default, and it is not a stub: it clones, builds the image,
// installs, runs the tests, writes the patch, verifies it and composes the
// body. Only the three public acts are withheld -- commit, push, and opening
// the pull request. Cloning a public repository is not a public action, and a
// dry run that skipped it could never tell anyone whether the rest works.
func implementCmd(root string, args []string) int {
	execute := false
	var slug string
	for _, a := range args {
		switch a {
		case "--execute":
			execute = true
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "unknown flag %q\n", a)
				return 2
			}
			slug = a
		}
	}
	if slug == "" {
		fmt.Fprintln(os.Stderr, "usage: pipeline implement <slug> [--execute]")
		return 2
	}

	ctx := context.Background()
	st := store.New(root)
	log := audit.New(root)

	c, err := st.Load(slug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	// The human gate guards --execute, not the dry run.
	//
	// A dry run publishes nothing and transitions nothing; its entire purpose
	// is to let a person see what a patch would look like before deciding
	// whether to approve it. Requiring approval first would invert that --
	// the user would have to authorise a pull request in order to find out
	// what it would say.
	//
	// --execute is different, and there the gate is absolute: only a person,
	// or internal/autogate under its own guardrails, can put a candidate in a
	// state this will submit from.
	switch {
	case execute && c.Status != model.StatusApproved && c.Status != model.StatusAutoApproved:
		fmt.Fprintf(os.Stderr, "%s is %q; --execute runs only on approved work.\n", slug, c.Status)
		if c.Status == model.StatusAbandoned {
			// The common case by far: an earlier attempt failed after the
			// gate. Saying which command undoes that is cheaper than leaving
			// the user to find the one status that is recoverable.
			fmt.Fprintf(os.Stderr, "An earlier attempt stopped here. "+
				"`pipeline retry %s <reason>` puts it back in the queue.\n", slug)
		}
		fmt.Fprintln(os.Stderr, "Run without --execute to see what the patch would be.")
		return 1
	case !execute && c.Status == model.StatusRejected:
		fmt.Fprintf(os.Stderr, "%s was rejected: %s\n", slug, c.RejectReason)
		return 1
	}

	cfg, err := policy.Load(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if execute {
		// Only the execute path. A halt stops what reaches other people, and
		// a dry run reaches nobody -- being able to see what a patch would
		// look like while the pipeline is stopped is useful, not dangerous.
		if reason, active := halt.New(root).Active(); active {
			fmt.Fprintf(os.Stderr, "HALT is set (%s); not submitting\n", reason)
			return 1
		}
		// Checked before the container work as well as after it. An hour of
		// building an image to be told the day's budget was already spent is
		// a waste, and the answer is almost always the same at both ends.
		all, _ := st.All()
		if held := cfg.Policy.Caps.Check(all, c, time.Now()); len(held) > 0 {
			fmt.Fprintf(os.Stderr, "not submitting %s:\n", c.Slug())
			for _, h := range held {
				fmt.Fprintln(os.Stderr, "  "+h)
			}
			return 1
		}
		// One run at a time. Two concurrent submissions would each see the
		// other's slot as free and both open a pull request.
		l := lock.New(root)
		if err := l.Acquire("implement " + slug); err != nil {
			if errors.Is(err, lock.ErrBusy) {
				fmt.Fprintln(os.Stderr, "skipped:", err)
				return 0
			}
			fmt.Fprintln(os.Stderr, "lock:", err)
			return 1
		}
		defer l.Release()
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
	rm := &repo.Manager{Root: root, ID: id, Env: env, Log: say}
	gh := ghx.New(env)
	brain := llm.New(env)

	say(fmt.Sprintf("preparing %s", c.Repo))
	clone, err := rm.Prepare(ctx, c.Repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clone:", err)
		return 1
	}
	branch, err := rm.Branch(ctx, clone, c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "branch:", err)
		return 1
	}
	c.Branch = branch
	say("branch " + branch)

	// --- environment ----------------------------------------------------
	overrides, err := recipe.LoadOverrides(filepath.Join(root, "config", "environments.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "environments.yaml:", err)
		return 1
	}
	var ov *recipe.Override
	if o, ok := overrides[c.Repo]; ok {
		ov = &o
	}
	lang := ""
	if c.Facts != nil {
		lang = c.Facts.PrimaryLanguage
	}
	rec, err := recipe.Resolve(clone, c.Repo, lang, ov, toolchainCommands())
	if err != nil && !errors.Is(err, recipe.ErrIncomplete) {
		fmt.Fprintln(os.Stderr, "recipe:", err)
		return 1
	}
	say(fmt.Sprintf("recipe: %s", rec))
	for _, u := range rec.Unresolved {
		say("  ? " + u)
	}
	if !rec.Complete() {
		fmt.Fprintf(os.Stderr, "\nno usable recipe for %s: nothing can be verified here.\n"+
			"Add an entry to config/environments.yaml -- the unresolved lines above say what is missing.\n", c.Repo)
		return 1
	}

	built, err := image.Ensure(ctx, rec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "image:", err)
		return 1
	}
	say(fmt.Sprintf("image: %s (cached=%v, %s)", built.Tag, built.Cached,
		built.Duration.Round(time.Second)))

	lim := sandbox.DefaultLimits()
	lim.Timeout = 30 * time.Minute
	sess, err := sandbox.Start(ctx, sandbox.Spec{
		Image: built.Tag, Clone: clone, Network: true,
		Volumes: rec.Volumes(), Platform: rec.Platform, Limits: lim,
		Labels: map[string]string{"ossp.repo": recipe.Slug(c.Repo)},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox:", err)
		return 1
	}
	defer sess.Close()

	say("installing dependencies (network on)")
	if _, err := repro.Install(ctx, sess, rec); err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}

	// --- baseline -------------------------------------------------------
	// Before the patch, so only NEW failures are attributed to it. A container
	// will never match a maintainer's CI exactly, and without this every
	// pre-existing red test looks like something this change broke.
	if err := sess.Detach(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "detach:", err)
		return 1
	}
	say("baseline on the unmodified checkout (network off)")
	baseline, err := repro.Verify(ctx, sess, rec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "baseline:", err)
		return 1
	}
	for _, r := range baseline {
		say("  " + r.String())
	}
	if pre := repro.PreexistingFailures(baseline); len(pre) > 0 {
		say(fmt.Sprintf("  %d test(s) already failing before any change", len(pre)))
	}

	// --- the patch ------------------------------------------------------
	body := ""
	if p, err := harvest.Load(root, c.Slug()); err == nil && p.Issue != nil {
		body = p.Issue.Body
	}
	say("writing the patch (agent on the host, no shell)")
	res, err := implement.Run(ctx, brain, brain, rm, netRunner{sess}, sess, c, clone,
		baseline, implement.Options{Body: body, Log: say})
	if err != nil {
		fmt.Fprintln(os.Stderr, "implement:", err)
		return 1
	}
	_ = log.Record("implement", c.Slug(), fmt.Sprintf("ok=%v toolchain=%s", res.OK, res.Toolchain))

	if res.DiffEmpty {
		fmt.Printf("\nNo change made. The agent's reason:\n\n%s\n", res.Refusal)
		return 1
	}
	say("scope: " + res.TestScope)
	for _, r := range res.Tests {
		say("  " + r.String())
	}
	if n := implement.NewFailures(baseline, res); len(n) > 0 {
		fmt.Printf("\nfailures this patch introduced:\n")
		for _, f := range n {
			fmt.Printf("  %s\n", f)
		}
	}
	for _, b := range res.Blocked {
		fmt.Printf("BLOCKED: %s\n", b)
	}

	// Does the added test actually exercise the bug? "The tests pass" proves
	// nothing on its own: a test that passes with or without the source change
	// looks like verification and is worth nothing.
	patchFile := filepath.Join(root, "state", "context", c.Slug()+".patch")
	_ = os.MkdirAll(filepath.Dir(patchFile), 0o755)
	ff, ferr := implement.ConfirmTestFailsFirst(ctx, rm, rm, sess, clone, patchFile, cmdsOf(res))
	switch {
	case ferr != nil:
		fmt.Fprintln(os.Stderr, "fails-first check:", ferr)
		return 1
	case !ff.Checked:
		say("fails-first: not checked -- " + ff.Why)
	case ff.FailedWithoutTheFix:
		say("fails-first: confirmed -- " + ff.Why)
	default:
		fmt.Printf("BLOCKED: %s\n", ff.Why)
		res.Blocked = append(res.Blocked, ff.Why)
	}

	// --- the last gate --------------------------------------------------
	// Before Preflight, not after: the commit stages everything with `git add
	// -A`, so a __pycache__ the test run created is part of the shipment
	// unless it is gone by now. Running it in a dry run too is what makes the
	// dry run's verdict the same verdict --execute would reach.
	if removed, err := submit.CleanArtefacts(ctx, rm, clone); err != nil {
		fmt.Fprintln(os.Stderr, "clean:", err)
		return 1
	} else if len(removed) > 0 {
		say("cleaned " + strings.Join(removed, ", "))
	}

	problems, err := submit.Preflight(ctx, rm, clone, submit.Identity{
		Name: id.Name, Email: id.Email, Login: id.Login, Private: id.PrivateStrings(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "preflight:", err)
		return 1
	}
	for _, p := range problems {
		fmt.Printf("PREFLIGHT: %s\n", p.Why)
	}

	verification := res.TestScope
	sid := submit.Identity{Name: id.Name, Email: id.Email, Login: id.Login,
		Private: id.PrivateStrings()}
	plan := submit.Prepare(ctx, brain, rm, c, clone, verification, sid)

	fmt.Printf("\n--- pull request ---\ntitle: %s\nbase:  %s\nhead:  %s\n\n%s\n",
		plan.Title, plan.Base, plan.Head, plan.Body)

	// Open re-checks the body immediately before sending it and refuses on a
	// match. Reporting that here is what makes a dry run's verdict the verdict
	// --execute would reach: without it, the one thing a dry run exists to
	// show you is the one thing it does not check.
	if bad := guard.CheckBody(plan.Body); len(bad) > 0 {
		fmt.Printf("\nBODY: mentions %s -- submitting would refuse this\n",
			strings.Join(bad, ", "))
		res.Blocked = append(res.Blocked, "the pull request body mentions "+
			strings.Join(bad, ", "))
	}

	if !execute {
		fmt.Println("[dry run] nothing was committed, pushed or opened.")
		return 0
	}
	if len(problems) > 0 || len(res.Blocked) > 0 {
		fmt.Fprintln(os.Stderr, "refusing to submit: the problems above must clear first")
		return 1
	}
	pub := publisher{root: root, caps: cfg.Policy.Caps, st: st, log: log,
		git: rm, gh: gh, brain: brain, id: sid}
	return pub.submit(ctx, c, clone, plan, res.Toolchain)
}

// publisher performs the three acts a dry run withholds: commit, push, open.
//
// Each one is recorded on disk before the next is attempted, so a crash or a
// lost network leaves a status that says what actually happened. That
// ordering is the point: the watcher reads these statuses and refuses to touch
// a clone that is ahead of the fork, so "pushed" written around a push that
// failed is not a cosmetic inaccuracy.
//
// The fields are the narrow interfaces this sequence uses, not the clients it
// happens to be handed. That is what makes the sequence testable at all: the
// ordering below is the whole safety argument, and an argument nothing checks
// is one that quietly stops being true.
type publisher struct {
	root  string
	caps  policy.Caps
	st    prStore
	log   recorder
	git   prGit
	gh    prGH
	brain submit.Judge
	id    submit.Identity
}

type prStore interface {
	All() ([]*model.Candidate, []store.LoadResult)
	Save(c *model.Candidate) (string, error)
}

type recorder interface {
	Record(kind, slug, detail string) error
}

// prGit is git plus the two things only the manager can do: create the fork,
// and say whether the clone is still acting as the OSS account.
type prGit interface {
	submit.Git
	EnsureFork(ctx context.Context, gh repo.Forker, repo, dir string) (string, error)
	AssertIdentity(clone string) error
}

// prGH is one method under two names. submit needs it to open the pull
// request, repo needs it to create the fork, and neither should have to learn
// about the other's interface to be given the same client.
type prGH interface {
	submit.GH
	repo.Forker
}

func (p publisher) submit(ctx context.Context, c *model.Candidate, clone string,
	plan submit.Plan, toolchain string) int {

	save := func(next model.Status, note string) error {
		if err := model.Transition(c, next, note); err != nil {
			// Our own ordering being wrong is categorically different from a
			// network blip, and v1 swallowed exactly this and lost a merge.
			_ = p.log.Record("transition_error", c.Slug(), err.Error())
			return err
		}
		_, err := p.st.Save(c)
		return err
	}

	// Re-checked here rather than only at the start. Building an image and
	// running a test suite takes long enough for the watcher to have opened,
	// merged or closed something in the meantime, and the cap that matters is
	// the one true at the moment the pull request appears.
	all, _ := p.st.All()
	if held := p.caps.Check(all, c, time.Now()); len(held) > 0 {
		fmt.Fprintf(os.Stderr, "\nnot submitting %s:\n", c.Slug())
		for _, h := range held {
			fmt.Fprintln(os.Stderr, "  "+h)
		}
		fmt.Fprintln(os.Stderr, "The patch is kept; it stays approved and the next run retries.")
		return 1
	}

	// Before the push, not only inside Open. Open re-checks as the last
	// backstop, but by the time it runs the branch is already on the fork --
	// and a body that would be refused should not have cost a public branch
	// that no pull request will ever explain.
	if bad := guard.CheckBody(plan.Body); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "refusing to submit: the body mentions %s\n",
			strings.Join(bad, ", "))
		return 1
	}

	if err := save(model.StatusImplementing, "verified locally; submitting"); err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
		return 1
	}

	// The fork is created here and nowhere earlier: a fork is publicly visible
	// on the account's profile and in the upstream's fork list, which makes it
	// one of the acts a dry run must not perform.
	fork, err := p.git.EnsureFork(ctx, p.gh, c.Repo, clone)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fork:", err)
		_ = save(model.StatusAbandoned, "could not prepare the fork: "+oneLine(err.Error()))
		return 1
	}
	_ = p.log.Record("fork", c.Slug(), c.Repo+" -> "+fork)
	say("fork " + fork)

	// Re-asserted now that a remote has been added. HardenClone ran before the
	// fork existed, so this is the first moment the push destination can be
	// checked at all -- and it is the last moment before a branch leaves this
	// machine under a name that must not be linkable to the user.
	if err := p.git.AssertIdentity(clone); err != nil {
		fmt.Fprintln(os.Stderr, "identity:", err)
		_ = save(model.StatusAbandoned, "identity assertion failed before push")
		return 1
	}

	trailers, err := guard.LoadCommitPatterns(
		filepath.Join(p.root, "config", "forbidden-trailers.txt"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		_ = save(model.StatusAbandoned, "commit guard could not be loaded")
		return 1
	}
	msg := submit.CommitMessage(ctx, p.brain, p.git, c, clone, p.id)
	if bad := trailers.Check(msg); len(bad) > 0 {
		// The standing rule, enforced at the last possible moment rather than
		// trusted to the prompt: git history is permanent and public, and a
		// commit message cannot be edited after a maintainer has pulled it.
		fmt.Fprintln(os.Stderr, "refusing to commit; the message matched:")
		for _, b := range bad {
			fmt.Fprintln(os.Stderr, "  "+b)
		}
		_ = save(model.StatusAbandoned, "commit message failed the guard")
		return 1
	}
	if err := submit.Commit(ctx, p.git, clone, msg); err != nil {
		fmt.Fprintln(os.Stderr, "commit:", err)
		_ = save(model.StatusAbandoned, "commit rejected: "+oneLine(err.Error()))
		return 1
	}
	say("committed: " + firstLine(msg))
	if err := save(model.StatusImplemented, "committed ("+toolchain+")"); err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
		return 1
	}

	if err := submit.Push(ctx, p.git, clone, plan); err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		// Abandoned rather than left at implemented, because a re-run builds
		// the branch again from origin and would discard the local commit
		// anyway. Nothing is lost: the verified patch is on disk beside the
		// candidate, and `pipeline retry` starts the whole run over cleanly.
		_ = save(model.StatusAbandoned, "push failed: "+oneLine(err.Error()))
		fmt.Fprintf(os.Stderr, "Nothing reached GitHub. `pipeline retry %s <reason>` "+
			"re-runs it once the cause is fixed.\n", c.Slug())
		return 1
	}
	_ = p.log.Record("push", c.Slug(), fork+" "+plan.Branch)
	if err := save(model.StatusPushed, "pushed to "+fork); err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
		return 1
	}
	say("pushed " + fork + " " + plan.Branch)

	url, err := submit.Open(ctx, p.gh, c, plan)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		if url != "" {
			// Opened, but its number could not be read. Say the URL rather
			// than leaving a live pull request the pipeline has lost track of.
			fmt.Fprintln(os.Stderr, "a pull request IS open at", url)
		}
		// Left at pushed, which is the truth: the branch is public. This is
		// the one failure the pipeline cannot finish on its own -- a re-run
		// would rebuild the branch from origin, and the watcher has no pull
		// request number to follow -- so it says plainly what is where.
		fmt.Fprintf(os.Stderr, "The branch IS on %s as %s. "+
			"Open the pull request by hand, or `pipeline reject %s <reason>` to drop it.\n",
			fork, plan.Branch, c.Slug())
		return 1
	}
	_ = p.log.Record("pr_open", c.Slug(), url)
	// The number goes to disk before the status does. A pull request whose
	// number was never recorded is invisible to the watcher for good -- Sync
	// returns "no PR" and stops -- so if only one of the two writes can
	// survive, it has to be this one.
	if _, err := p.st.Save(c); err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
	}
	if err := save(model.StatusPROpen, url); err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
		fmt.Fprintln(os.Stderr, "the pull request IS open at", url)
		return 1
	}
	fmt.Printf("\nopened %s\n", url)
	return 0
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// netRunner is the session while it still has a network. Passing the same
// session as both arguments would be a mistake implement's two-parameter
// signature exists to make visible, so the wrapper is named for what it is.
type netRunner struct{ s *sandbox.Session }

func (n netRunner) Run(ctx context.Context, cmd string) (sandbox.Result, error) {
	return n.s.Run(ctx, cmd)
}

func say(s string) { fmt.Println("  " + s) }

// cmdsOf is the test commands a run actually executed, for re-running them
// against the reverted source.
func cmdsOf(r implement.Result) []string {
	var out []string
	for _, t := range r.Tests {
		if t.Outcome != repro.Environment {
			out = append(out, t.Command)
		}
	}
	return out
}
