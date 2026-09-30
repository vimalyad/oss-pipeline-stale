package main

import (
	"context"
	"strconv"

	"github.com/vimalyad/osspipeline/internal/audit"
	"github.com/vimalyad/osspipeline/internal/brief"
	"github.com/vimalyad/osspipeline/internal/cilog"
	"github.com/vimalyad/osspipeline/internal/contest"
	"github.com/vimalyad/osspipeline/internal/discover"
	"github.com/vimalyad/osspipeline/internal/gate"
	"github.com/vimalyad/osspipeline/internal/ghx"
	"github.com/vimalyad/osspipeline/internal/harvest"
	"github.com/vimalyad/osspipeline/internal/implement"
	"github.com/vimalyad/osspipeline/internal/llm"
	"github.com/vimalyad/osspipeline/internal/profile"
	"github.com/vimalyad/osspipeline/internal/propose"
	"github.com/vimalyad/osspipeline/internal/publish"
	"github.com/vimalyad/osspipeline/internal/recipe"
	"github.com/vimalyad/osspipeline/internal/replies"
	"github.com/vimalyad/osspipeline/internal/repo"
	"github.com/vimalyad/osspipeline/internal/repofacts"
	"github.com/vimalyad/osspipeline/internal/sandbox"
	"github.com/vimalyad/osspipeline/internal/store"
	"github.com/vimalyad/osspipeline/internal/submit"
	"github.com/vimalyad/osspipeline/internal/toolchain"
	"github.com/vimalyad/osspipeline/internal/watch"
)

// This file is where the architecture is enforced.
//
// Every package declares the narrow interface it needs rather than importing a
// client, which keeps the dependencies one-way but means nothing proves the
// real types still satisfy those interfaces -- until here. These assertions
// fail the build, not a test.
//
// That is the concrete reason this pipeline is in Go. Its predecessor put the
// same seams in one module of Protocols, and because Python does not enforce
// them, four of the eight had drifted to describe functions that no longer
// existed. Nothing broke when the code moved away from the design, so nobody
// found out.
var (
	_ watch.API             = (*ghx.Client)(nil)
	_ watch.CheckClassifier = (*cilog.Fetcher)(nil)
	_ watch.Saver           = (*store.Store)(nil)
	_ replies.Drafter       = (*llm.Client)(nil)
	_ brief.Judge           = (*llm.Client)(nil)
	_ implement.Agent       = (*llm.Client)(nil)
	_ implement.Judge       = (*llm.Client)(nil)
	_ implement.Git         = (*repo.Manager)(nil)
	_ implement.Runner      = (*sandbox.Session)(nil)
	_ submit.Git            = (*repo.Manager)(nil)
	_ submit.Judge          = (*llm.Client)(nil)
	_ submit.GH             = (*ghx.Client)(nil)
	_ repo.Forker           = (*ghx.Client)(nil)
	_ discover.API          = (*ghx.Client)(nil)
	_ contest.API           = (*ghx.Client)(nil)
	_ harvest.API           = (*ghx.Client)(nil)
	_ propose.Domains       = (*profile.Profile)(nil)
	_ gate.Store            = (*store.Store)(nil)
	_ gate.Auditor          = (*audit.Log)(nil)
	_ publish.API           = (*ghx.Client)(nil)
	_ cilog.API             = cilogAPI{}
	_ repofacts.API         = plainGet{}
	_ repofacts.Judge       = (*llm.Client)(nil)
	_ repofacts.Cache       = (*store.Store)(nil)
	_ recipe.Commands       = recipe.CommandsFunc(nil)
	_ replies.Commenter     = prCommenter{}
)

// cilogAPI adapts the gh client to cilog's own options type.
//
// cilog states what it needs from a transport instead of importing one, so
// that it stays testable against captured logs with no network at all. The
// cost is this adapter, and the wiring layer is where it belongs: neither
// package should learn about the other.
type cilogAPI struct{ c *ghx.Client }

func (a cilogAPI) REST(ctx context.Context, path string, o cilog.RESTOptions) (string, error) {
	return a.c.REST(ctx, path, ghx.RESTOptions{
		Method: o.Method, Paginate: o.Paginate, JQ: o.JQ, Fields: o.Fields,
	})
}

// plainGet narrows the client to the two calls repofacts makes. The package
// states what it needs rather than importing the client, which is what lets it
// be tested against a map of canned responses instead of GitHub.
type plainGet struct{ c *ghx.Client }

func (p plainGet) Get(ctx context.Context, path string) (string, error) {
	return p.c.REST(ctx, path, ghx.RESTOptions{})
}

func (p plainGet) GraphQL(ctx context.Context, q string, vars map[string]any, v any) error {
	return p.c.GraphQL(ctx, q, vars, v)
}

// prCommenter posts a comment on a pull request.
//
// Deliberately its own type rather than a method on the gh client: the thing
// that drafts a reply and the thing that publishes one must not be reachable
// through the same object, because the whole reason replies are queued is that
// nothing writes under the user's name without a person saying so.
type prCommenter struct{ c *ghx.Client }

func (p prCommenter) Comment(ctx context.Context, repo string, pr int, body string) (string, error) {
	return p.c.RESTRaw(ctx, []string{
		"pr", "comment", strconv.Itoa(pr), "--repo", repo, "--body", body,
	}, "")
}

// toolchainCommands is the seam between recipe and toolchain. recipe asks what
// commands exercise a clone; toolchain answers without ever learning that
// containers exist.
func toolchainCommands() recipe.Commands {
	return recipe.CommandsFunc(func(root string) (test, lint, install, masks []string) {
		for _, t := range toolchain.Detect(root) {
			test = append(test, t.Test...)
			lint = append(lint, t.Lint...)
			install = append(install, t.Install...)
		}
		return test, lint, install, nil
	})
}

// sandboxRunner is the only type permitted to execute a target repository's
// code, asserted here so a second one cannot appear unnoticed.
var _ interface {
	Run(ctx context.Context, cmd string) (sandbox.Result, error)
} = (*sandbox.Session)(nil)
