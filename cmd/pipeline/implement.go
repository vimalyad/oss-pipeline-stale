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
	"github.com/vimalyad/osspipeline/internal/harvest"
	"github.com/vimalyad/osspipeline/internal/identity"
	"github.com/vimalyad/osspipeline/internal/image"
	"github.com/vimalyad/osspipeline/internal/implement"
	"github.com/vimalyad/osspipeline/internal/llm"
	"github.com/vimalyad/osspipeline/internal/model"
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
		fmt.Fprintf(os.Stderr, "%s is %q; --execute runs only on approved work.\n"+
			"Run without --execute to see what the patch would be.\n", slug, c.Status)
		return 1
	case !execute && c.Status == model.StatusRejected:
		fmt.Fprintf(os.Stderr, "%s was rejected: %s\n", slug, c.RejectReason)
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
	rm := &repo.Manager{Root: root, ID: id, Env: env, Log: say}
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

	// --- the last gate --------------------------------------------------
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

	if !execute {
		fmt.Println("[dry run] nothing was committed, pushed or opened.")
		return 0
	}
	if len(problems) > 0 || len(res.Blocked) > 0 {
		fmt.Fprintln(os.Stderr, "refusing to submit: the problems above must clear first")
		return 1
	}
	fmt.Fprintln(os.Stderr, "submitting is not wired up yet; run without --execute")
	return 1
}

// netRunner is the session while it still has a network. Passing the same
// session as both arguments would be a mistake implement's two-parameter
// signature exists to make visible, so the wrapper is named for what it is.
type netRunner struct{ s *sandbox.Session }

func (n netRunner) Run(ctx context.Context, cmd string) (sandbox.Result, error) {
	return n.s.Run(ctx, cmd)
}

func say(s string) { fmt.Println("  " + s) }
