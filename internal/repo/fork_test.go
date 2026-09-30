package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vimalyad/osspipeline/internal/ghx"
)

// fakeGH replays a script of answers in order, checking as it goes that each
// call is the one the script expected. Order matters here: the same lookup is
// made before and after the fork is created, and a fake that matched on the
// arguments alone could not tell those two apart.
type fakeGH struct {
	replies []reply
	calls   [][]string
	t       *testing.T
}

type reply struct {
	match string
	out   string
	err   error
}

func (f *fakeGH) RESTRaw(_ context.Context, args []string, _ string) (string, error) {
	f.calls = append(f.calls, args)
	key := strings.Join(args, " ")
	if len(f.replies) == 0 {
		// Unscripted: the resource is not there. That is the honest default,
		// since every call this package makes is a lookup or a create.
		return "", fmt.Errorf("%w: gh %s", ghx.ErrNotFound, key)
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	if !strings.Contains(key, r.match) {
		f.t.Fatalf("expected a call matching %q, got %q", r.match, key)
	}
	return r.out, r.err
}

func (f *fakeGH) called(sub string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			n++
		}
	}
	return n
}

func forkJSON(parent string) string {
	return fmt.Sprintf(`{"fork":true,"parent":{"full_name":%q}}`, parent)
}

func noPolling(t *testing.T) {
	t.Helper()
	old := ForkPollInterval
	ForkPollInterval = time.Millisecond
	t.Cleanup(func() { ForkPollInterval = old })
}

func TestEnsureForkReusesAnExistingFork(t *testing.T) {
	root := t.TempDir()
	m := manager(t, root)
	dir := clone(t, m, upstream(t))
	gh := &fakeGH{t: t, replies: []reply{{match: "repos/someone/widget", out: forkJSON("acme/widget")}}}

	fork, err := m.EnsureFork(context.Background(), gh, "acme/widget", dir)
	if err != nil {
		t.Fatal(err)
	}
	if fork != "someone/widget" {
		t.Fatalf("fork = %q", fork)
	}
	if gh.called("--method POST") != 0 {
		t.Fatal("forked a repository that already existed")
	}
	if got := remoteURL(t, m, dir, "fork"); got != "https://github.com/someone/widget.git" {
		t.Fatalf("fork remote = %q", got)
	}
}

func TestEnsureForkCreatesAndWaits(t *testing.T) {
	noPolling(t)
	root := t.TempDir()
	m := manager(t, root)
	dir := clone(t, m, upstream(t))
	// Absent, absent, then present: GitHub answers the POST before the fork
	// is usable, and a push to a repository that is not there yet fails.
	gh := &fakeGH{t: t, replies: []reply{
		{match: "repos/someone/widget", err: fmt.Errorf("%w: gh api", ghx.ErrNotFound)},
		{match: "repos/acme/widget/forks --method POST", out: "{}"},
		{match: "repos/someone/widget", err: fmt.Errorf("%w: gh api", ghx.ErrNotFound)},
		{match: "repos/someone/widget", out: forkJSON("acme/widget")},
	}}

	fork, err := m.EnsureFork(context.Background(), gh, "acme/widget", dir)
	if err != nil {
		t.Fatal(err)
	}
	if fork != "someone/widget" {
		t.Fatalf("fork = %q", fork)
	}
	if gh.called("--method POST") != 1 {
		t.Fatalf("expected exactly one POST, got %d", gh.called("--method POST"))
	}
	if got := remoteURL(t, m, dir, "fork"); got != "https://github.com/someone/widget.git" {
		t.Fatalf("fork remote = %q", got)
	}
}

func TestEnsureForkRefusesAnUnrelatedRepositoryOfTheSameName(t *testing.T) {
	root := t.TempDir()
	m := manager(t, root)
	dir := clone(t, m, upstream(t))
	for _, body := range []string{
		forkJSON("someoneelse/widget"), // a fork of a different project
		`{"fork":false}`,               // our own repository, not a fork at all
	} {
		gh := &fakeGH{t: t, replies: []reply{{match: "repos/someone/widget", out: body}}}
		_, err := m.EnsureFork(context.Background(), gh, "acme/widget", dir)
		if !errors.Is(err, ErrFork) {
			t.Fatalf("%s was accepted: %v", body, err)
		}
		if gh.called("--method POST") != 0 {
			t.Fatal("tried to fork over an existing repository")
		}
		if remoteURL(t, m, dir, "fork") != "" {
			t.Fatal("wired up a remote pointing at the wrong repository")
		}
	}
}

// A revoked token, a rate limit or an outage must never be read as "the fork
// does not exist" -- that reading turns a transient failure into a POST, and
// the pipeline then waits sixty seconds for a fork it already had.
func TestEnsureForkDoesNotTreatEveryFailureAsAbsence(t *testing.T) {
	root := t.TempDir()
	m := manager(t, root)
	dir := clone(t, m, upstream(t))
	gh := &fakeGH{t: t, replies: []reply{
		{match: "repos/someone/widget", err: fmt.Errorf("%w: 403 rate limit exceeded", ghx.ErrRateLimited)},
	}}

	_, err := m.EnsureFork(context.Background(), gh, "acme/widget", dir)
	if !errors.Is(err, ErrFork) {
		t.Fatalf("err = %v", err)
	}
	if gh.called("--method POST") != 0 {
		t.Fatal("forked in response to a rate limit")
	}
}

func TestEnsureForkGivesUpIfTheForkNeverAppears(t *testing.T) {
	noPolling(t)
	old := ForkPollAttempts
	ForkPollAttempts = 3
	t.Cleanup(func() { ForkPollAttempts = old })

	root := t.TempDir()
	m := manager(t, root)
	dir := clone(t, m, upstream(t))
	gh := &fakeGH{t: t, replies: []reply{
		{match: "repos/someone/widget", err: fmt.Errorf("%w: gh api", ghx.ErrNotFound)},
		{match: "repos/acme/widget/forks --method POST", out: "{}"},
	}}

	if _, err := m.EnsureFork(context.Background(), gh, "acme/widget", dir); !errors.Is(err, ErrFork) {
		t.Fatalf("err = %v", err)
	}
	if remoteURL(t, m, dir, "fork") != "" {
		t.Fatal("wired up a remote for a fork that never appeared")
	}
}

func TestEnsureForkCorrectsAStaleRemote(t *testing.T) {
	root := t.TempDir()
	m := manager(t, root)
	dir := clone(t, m, upstream(t))
	if _, err := m.git(context.Background(), dir, "remote", "add", "fork",
		"https://github.com/someone-else/widget.git"); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGH{t: t, replies: []reply{{match: "repos/someone/widget", out: forkJSON("acme/widget")}}}

	if _, err := m.EnsureFork(context.Background(), gh, "acme/widget", dir); err != nil {
		t.Fatal(err)
	}
	if got := remoteURL(t, m, dir, "fork"); got != "https://github.com/someone/widget.git" {
		t.Fatalf("a remote left over from another account survived: %q", got)
	}
}

func remoteURL(t *testing.T, m *Manager, dir, name string) string {
	t.Helper()
	out, err := m.git(context.Background(), dir, "remote", "get-url", "--push", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
