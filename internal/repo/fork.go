package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/ghx"
)

var ErrFork = errors.New("fork")

// Forker is the GitHub surface this file needs: one call that runs gh and
// returns its stdout. Narrow on purpose -- creating a fork is the only write
// this package makes to anyone's account, and the interface says so.
//
// An implementation must report a missing repository as an error wrapping
// ghx.ErrNotFound. That distinction is load-bearing: "absent" means create the
// fork, and everything else -- a revoked token, a rate limit -- must not be
// mistaken for it.
type Forker interface {
	RESTRaw(ctx context.Context, args []string, stdin string) (string, error)
}

// ForkPollInterval and ForkPollAttempts bound the wait for an asynchronous
// fork. GitHub answers the POST immediately and populates the repository
// afterwards, so the returned name is not usable until it resolves.
var (
	ForkPollInterval = 2 * time.Second
	ForkPollAttempts = 30
)

// EnsureFork makes sure the OSS account has a fork of repo and that the clone
// has it wired up as the `fork` remote. It returns the fork's full name.
//
// Creating a fork is a public act -- the repository appears on the account's
// profile and in the upstream's fork list -- so this is never called by a dry
// run, and never by anything but the execute path.
func (m *Manager) EnsureFork(ctx context.Context, gh Forker, repo, dir string) (string, error) {
	_, name, ok := strings.Cut(repo, "/")
	if !ok {
		return "", fmt.Errorf("%w: %q is not owner/name", ErrFork, repo)
	}
	fork := m.ID.Login + "/" + name

	switch existing, err := forkParent(ctx, gh, fork); {
	case err != nil:
		return "", err
	case existing == repo:
		// Already ours and already pointing at this upstream.
	case existing != "":
		// A fork of something else that happens to share a name. Pushing here
		// would put the branch on an unrelated project.
		return "", fmt.Errorf("%w: %s is a fork of %s, not of %s",
			ErrFork, fork, existing, repo)
	default:
		if err := m.createFork(ctx, gh, repo, fork); err != nil {
			return "", err
		}
	}

	if err := m.setForkRemote(ctx, dir, fork); err != nil {
		return "", err
	}
	return fork, nil
}

// forkParent returns the upstream a repository is a fork of. An empty string
// with no error means the repository does not exist yet, which is the normal
// case the first time a project is worked on.
func forkParent(ctx context.Context, gh Forker, fork string) (string, error) {
	out, err := gh.RESTRaw(ctx, []string{"api", "repos/" + fork}, "")
	if err != nil {
		if errors.Is(err, ghx.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("%w: reading %s: %v", ErrFork, fork, err)
	}
	var r struct {
		Fork   bool `json:"fork"`
		Parent struct {
			FullName string `json:"full_name"`
		} `json:"parent"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return "", fmt.Errorf("%w: reading %s: %v", ErrFork, fork, err)
	}
	if !r.Fork {
		// A repository of our own with the same name. Say which, rather than
		// treating it as a fork of nothing and trying to create one.
		return "not a fork", nil
	}
	return r.Parent.FullName, nil
}

func (m *Manager) createFork(ctx context.Context, gh Forker, repo, fork string) error {
	m.logf("    forking %s -> %s", repo, fork)
	// POST, not GET: a GET on this path lists a repository's existing forks
	// and succeeds without creating anything.
	if _, err := gh.RESTRaw(ctx,
		[]string{"api", "repos/" + repo + "/forks", "--method", "POST"}, ""); err != nil {
		return fmt.Errorf("%w: creating %s: %v", ErrFork, fork, err)
	}
	for i := 0; i < ForkPollAttempts; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ForkPollInterval):
		}
		if parent, err := forkParent(ctx, gh, fork); err == nil && parent == repo {
			return nil
		}
	}
	return fmt.Errorf("%w: %s did not become available after %s",
		ErrFork, fork, time.Duration(ForkPollAttempts)*ForkPollInterval)
}

// setForkRemote points the `fork` remote at the fork, adding it if absent and
// correcting it if it is stale. set-url rather than add-or-leave: a clone kept
// from an earlier run may have a remote pointing at a different account.
func (m *Manager) setForkRemote(ctx context.Context, dir, fork string) error {
	url := "https://github.com/" + fork + ".git"
	remotes, err := m.git(ctx, dir, "remote")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFork, err)
	}
	verb := "add"
	for _, r := range nonEmptyLines(remotes) {
		if strings.TrimSpace(r) == "fork" {
			verb = "set-url"
			break
		}
	}
	if _, err := m.git(ctx, dir, "remote", verb, "fork", url); err != nil {
		return fmt.Errorf("%w: git remote %s: %v", ErrFork, verb, err)
	}
	return nil
}
