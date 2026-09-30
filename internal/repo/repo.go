// Package repo manages the working clones of target repositories.
//
// Clones are long-lived and reused. They are also the one place where this
// pipeline's identity guarantees have to hold against a repository that can
// rewrite git configuration out from under us, so every clone is hardened when
// it is made and re-checked before anything is committed or pushed.
package repo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/identity"
	"github.com/vimalyad/osspipeline/internal/model"
)

// Sanity limits. A bug fix touches a handful of files. If the working tree
// claims to differ from HEAD across a large fraction of the repository, the
// clone is broken rather than the patch being enormous -- one repo cloned
// during a network outage reported all 708 tracked files as modified, and the
// test selector duly handed a licence file to the test runner.
const (
	SanityMaxFraction = 0.30
	SanityMaxFiles    = 150
)

// TakeoverMaxAgeDays is how old a prior author's work can be before rebuilding
// on it stops being a takeover and becomes an archaeology project. Past this,
// start from the default branch and credit them in the PR body instead.
const TakeoverMaxAgeDays = 365

type Manager struct {
	Root string
	ID   identity.Identity
	Env  []string
	Log  func(string)
}

func (m *Manager) logf(f string, a ...any) {
	if m.Log != nil {
		m.Log(fmt.Sprintf(f, a...))
	}
}

// Dir is where a candidate's repository is cloned.
func (m *Manager) Dir(repo string) string {
	return filepath.Join(m.Root, "work", strings.ReplaceAll(repo, "/", "__"))
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = m.Env
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args[:min(2, len(args))], " "), truncate(stderr, 300))
	}
	return strings.TrimSpace(string(out)), nil
}

// DefaultBranch is the upstream default, read from the remote rather than
// assumed: `main` and `master` are both live across the watchlist.
func (m *Manager) DefaultBranch(ctx context.Context, dir string) string {
	out, err := m.git(ctx, dir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil {
		if _, br, ok := strings.Cut(out, "refs/remotes/origin/"); ok {
			return br
		}
	}
	for _, guess := range []string{"main", "master"} {
		if _, err := m.git(ctx, dir, "rev-parse", "--verify", "origin/"+guess); err == nil {
			return guess
		}
	}
	return "main"
}

// Sanity reports whether a clone is in a state worth reasoning about.
func (m *Manager) Sanity(ctx context.Context, dir string) (bool, string) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false, "no .git directory"
	}
	tracked, err := m.git(ctx, dir, "ls-files")
	if err != nil {
		return false, "cannot list tracked files: " + err.Error()
	}
	nTracked := len(nonEmptyLines(tracked))
	if nTracked == 0 {
		return false, "no tracked files"
	}
	changed, _ := m.git(ctx, dir, "diff", "--name-only", "HEAD")
	nChanged := len(nonEmptyLines(changed))
	if nChanged > SanityMaxFiles || float64(nChanged) > float64(nTracked)*SanityMaxFraction {
		return false, fmt.Sprintf(
			"%d of %d tracked files differ from HEAD -- the clone is broken, "+
				"not a patch this large", nChanged, nTracked)
	}
	return true, "ok"
}

// Prepare makes a clone ready to work in: cloned, hardened, and proven to be
// operating as the OSS identity before anything can be committed.
func (m *Manager) Prepare(ctx context.Context, repo string) (string, error) {
	dir := m.Dir(repo)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if ok, why := m.Sanity(ctx, dir); !ok {
			m.logf("    discarding broken clone: %s", why)
			if err := os.RemoveAll(dir); err != nil {
				return "", err
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		m.logf("    cloning %s", repo)
		// Blobless: full history, file contents fetched on demand. The history
		// is needed to match commit style and to rebase; the blobs mostly are
		// not.
		cmd := exec.CommandContext(ctx, "git", "clone", "--filter=blob:none",
			"https://github.com/"+repo+".git", dir)
		cmd.Env = m.Env
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("clone %s: %s", repo, truncate(string(out), 300))
		}
	}
	if err := identity.HardenClone(dir, m.ID); err != nil {
		return "", err
	}
	if err := identity.AssertClone(dir, m.ID); err != nil {
		return "", err
	}
	return dir, nil
}

// Branch creates the working branch for a candidate from current upstream.
func (m *Manager) Branch(ctx context.Context, dir string, c *model.Candidate) (string, error) {
	base := m.DefaultBranch(ctx, dir)
	if _, err := m.git(ctx, dir, "fetch", "-q", "origin", base); err != nil {
		return "", err
	}
	branch := fmt.Sprintf("fix/issue-%d", c.Issue)
	if _, err := m.git(ctx, dir, "checkout", "-f", "-B", branch, "origin/"+base); err != nil {
		return "", err
	}
	return branch, nil
}

// Takeover builds on a prior author's abandoned work when that is still sane,
// and otherwise starts clean and credits them in the PR body.
//
// The age limit matters: rebasing work last touched years ago produces a diff
// nobody can review, which helps neither the maintainers nor the original
// author.
func (m *Manager) Takeover(ctx context.Context, dir string, c *model.Candidate) (string, string, error) {
	base := m.DefaultBranch(ctx, dir)
	branch := fmt.Sprintf("fix/issue-%d", c.Issue)
	if _, err := m.git(ctx, dir, "fetch", "-q", "origin", base); err != nil {
		return "", "", err
	}

	fresh := func(why string) (string, string, error) {
		m.logf("    %s; starting fresh from %s (prior author still credited)", why, base)
		_, _ = m.git(ctx, dir, "merge", "--abort")
		if _, err := m.git(ctx, dir, "checkout", "-f", "-B", branch, "origin/"+base); err != nil {
			return "", "", err
		}
		return branch, why, nil
	}

	sig := c.PRSignal
	if sig == nil {
		return fresh("no prior PR recorded")
	}
	age := 0
	if sig.DaysSinceCommit != nil {
		age = *sig.DaysSinceCommit
	}
	if age > TakeoverMaxAgeDays {
		return fresh(fmt.Sprintf("PR #%d last moved %dy ago, too old to rebase",
			sig.Number, age/365))
	}
	if _, err := m.git(ctx, dir, "fetch", "-q", "origin",
		fmt.Sprintf("pull/%d/head:%s-prior", sig.Number, branch)); err != nil {
		return fresh(fmt.Sprintf("could not fetch PR #%d", sig.Number))
	}
	if _, err := m.git(ctx, dir, "checkout", "-B", branch, branch+"-prior"); err != nil {
		return fresh(fmt.Sprintf("could not check out PR #%d", sig.Number))
	}

	// A failed merge must not be ignored: it leaves MERGE_HEAD set and the
	// tree conflicted, and the next step would patch on top of that.
	_, mergeErr := m.git(ctx, dir, "merge", "--no-edit", "origin/"+base)
	if _, statErr := os.Stat(filepath.Join(dir, ".git", "MERGE_HEAD")); mergeErr != nil || statErr == nil {
		return fresh(fmt.Sprintf("merging %s into PR #%d conflicts", base, sig.Number))
	}
	m.logf("    based on PR #%d by @%s (authorship preserved)", sig.Number, sig.Author)
	return branch, fmt.Sprintf("Builds on the work of @%s in #%d.", sig.Author, sig.Number), nil
}

// NameStatus returns git's name-status listings the guard needs: what the
// branch committed versus upstream, and what the working tree has pending.
//
// Untracked files are included in the pending listing, synthesised as adds.
// `git diff --name-status HEAD` reports tracked changes only, so a file the
// implementer created but has not committed is invisible to it -- and the
// commit step runs `git add -A`, which sweeps exactly those files in. That is
// the same shape as the incident this guard exists for (the rule was right,
// the scope was wrong), one door along.
func (m *Manager) NameStatus(ctx context.Context, dir string) (committed, pending string) {
	base := m.DefaultBranch(ctx, dir)
	_, _ = m.git(ctx, dir, "fetch", "-q", "origin", base)
	committed, _ = m.git(ctx, dir, "diff", "--name-status", "origin/"+base+"...HEAD")
	pending, _ = m.git(ctx, dir, "diff", "--name-status", "HEAD")

	untracked, _ := m.git(ctx, dir, "ls-files", "--others", "--exclude-standard")
	for _, f := range nonEmptyLines(untracked) {
		if pending != "" {
			pending += "\n"
		}
		pending += "A\t" + f
	}
	return committed, pending
}

// Diff is the full patch a branch represents, committed and pending together.
func (m *Manager) Diff(ctx context.Context, dir string) string {
	base := m.DefaultBranch(ctx, dir)
	a, _ := m.git(ctx, dir, "diff", "origin/"+base+"...HEAD")
	b, _ := m.git(ctx, dir, "diff", "HEAD")
	return a + "\n" + b
}

// TopLevelEntries is what the repository had before this branch touched it.
func (m *Manager) TopLevelEntries(ctx context.Context, dir string) map[string]bool {
	out, _ := m.git(ctx, dir, "ls-tree", "--name-only", "HEAD")
	set := map[string]bool{}
	for _, l := range nonEmptyLines(out) {
		name, _, _ := strings.Cut(l, "/")
		set[name] = true
	}
	return set
}

// CommitStyle is a sample of recent subjects, so a generated message can match
// the conventions this project already uses rather than impose new ones.
func (m *Manager) CommitStyle(ctx context.Context, dir string) string {
	out, _ := m.git(ctx, dir, "log", "-15", "--format=%s")
	lines := nonEmptyLines(out)
	if len(lines) > 6 {
		lines = lines[:6]
	}
	if len(lines) == 0 {
		return "(no history read)"
	}
	return strings.Join(lines, "; ")
}

// Age reports how long ago a clone was last updated, for pruning.
func (m *Manager) Age(dir string) time.Duration {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return 0
	}
	return time.Since(st.ModTime())
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Git runs a git command in dir and returns stdout.
//
// Exported so internal/submit can do the handful of things it needs -- fetch,
// list untracked files, commit, push -- without this package growing a method
// per verb. The identity environment is applied here, which is the reason
// submit must not shell out to git itself: every commit and push has to carry
// the isolated account's name, email and credential helper, and a call that
// bypassed this would silently use the machine's default account.
func (m *Manager) Git(ctx context.Context, dir string, args ...string) (string, error) {
	return m.git(ctx, dir, args...)
}

// AssertIdentity re-checks that this clone is still configured to act as the
// OSS account. A method rather than a call to identity.AssertClone at the use
// site, because the manager is what owns the clone and knows which identity it
// was hardened for -- and because it lets the publishing sequence be tested
// without a real hardened checkout, while still being a required part of the
// interface rather than an optional hook that can be left nil.
func (m *Manager) AssertIdentity(clone string) error {
	return identity.AssertClone(clone, m.ID)
}
