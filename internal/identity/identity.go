// Package identity is the boundary that keeps this pipeline's public actions
// under one specific GitHub account.
//
// This machine's ambient git and gh configuration resolves to a different
// account: the global user.email is that account's noreply address, and the
// global credential helper serves its token to any HTTPS push. Identity here
// is therefore never inherited -- it is asserted per clone, before any commit
// or push, across four independent layers:
//
//  1. API auth   -- GH_TOKEN per process, overriding gh's stored account
//  2. Push auth  -- a per-clone credential helper, with the chain reset first
//  3. Commit id  -- per-clone user.name/user.email plus GIT_AUTHOR_*
//  4. Guards     -- shared git hooks that abort a wrong-identity push
//
// Layer 4 is not trusted alone: a repo's own tooling can rewrite
// core.hooksPath (husky does this on npm install) and silently disarm it,
// which is why AssertClone re-checks all of the above in-process.
package identity

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CredentialKey is the git config key whose chain decides which token is
// served to a push. Getting this wrong pushes as the wrong account.
const CredentialKey = "credential.https://github.com.helper"

// Error reports that a clone is not provably operating as the OSS identity.
// It carries every violation found, not just the first: when this fires in an
// unattended run, the whole picture is what makes it debuggable.
type Error struct{ Problems []string }

func (e *Error) Error() string {
	return fmt.Sprintf("identity not asserted:\n  - %s",
		strings.Join(e.Problems, "\n  - "))
}

// Identity is the account everything public happens under, plus the other
// accounts on this machine that must never be used by mistake.
type Identity struct {
	Login           string
	Name            string
	Email           string
	KeychainAccount string
	KeychainService string
	WorkLogin       string
	WorkEmail       string
	OtherLogins     []string
	OtherEmails     []string
	Root            string
}

func (i Identity) HelperPath() string { return filepath.Join(i.Root, "bin", "gh-token-helper") }
func (i Identity) HooksPath() string  { return filepath.Join(i.Root, "githooks") }

// Load parses config/identity.env.
//
// Hand-rolled rather than sourced through a shell: this is the
// security-critical path and it must not execute anything. The same file is
// read by the git hooks, which have no interpreter beyond sh -- hence the
// KEY="value" format rather than YAML.
//
// (v1 had this parsed correctly here, but read elsewhere by grepping the file,
// which matched a comment line and produced a corrupted commit author. One
// parser, used everywhere.)
func Load(path string) (Identity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("read identity: %w", err)
	}
	vals := map[string]string{}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[strings.TrimSpace(key)] = unquote(val)
	}

	root, err := filepath.Abs(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return Identity{}, err
	}
	id := Identity{
		Login:           vals["OSS_LOGIN"],
		Name:            vals["OSS_NAME"],
		Email:           vals["OSS_EMAIL"],
		KeychainAccount: vals["OSS_KEYCHAIN_ACCOUNT"],
		KeychainService: vals["OSS_KEYCHAIN_SERVICE"],
		WorkLogin:       vals["WORK_LOGIN"],
		WorkEmail:       vals["WORK_EMAIL"],
		OtherLogins:     strings.Fields(vals["OTHER_LOGINS"]),
		OtherEmails:     strings.Fields(vals["OTHER_EMAILS"]),
		Root:            root,
	}
	var missing []string
	for k, v := range map[string]string{
		"OSS_LOGIN": id.Login, "OSS_NAME": id.Name, "OSS_EMAIL": id.Email,
		"OSS_KEYCHAIN_ACCOUNT": id.KeychainAccount,
		"OSS_KEYCHAIN_SERVICE": id.KeychainService,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return Identity{}, fmt.Errorf("%s is missing %s", path, strings.Join(missing, ", "))
	}
	return id, nil
}

// unquote strips one layer of surrounding quotes and any trailing comment.
// A bare value keeps everything up to the first unquoted '#'.
func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && strings.Contains(v[1:], `"`)) ||
			(v[0] == '\'' && strings.Contains(v[1:], `'`)) {
			q := v[0]
			if end := strings.IndexByte(v[1:], q); end >= 0 {
				return v[1 : 1+end]
			}
		}
	}
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// Token reads the PAT from the macOS Keychain. Never cached, never written to
// disk, never placed in a file this process controls.
func Token(id Identity) (string, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-a", id.KeychainAccount, "-s", id.KeychainService, "-w").Output()
	if err != nil {
		return "", fmt.Errorf(
			"no Keychain item %s/%s; create it with:\n"+
				"  security add-generic-password -a %s -s %s -w",
			id.KeychainService, id.KeychainAccount, id.KeychainAccount, id.KeychainService)
	}
	return strings.TrimSpace(string(out)), nil
}

// Env is the environment for subprocesses that talk to GitHub or write
// commits as us -- our own git and gh calls, and nothing else.
//
// GH_TOKEN takes precedence over gh's keyring, so this overrides the machine's
// active account per process. That is the whole reason `gh auth switch` is
// banned: it mutates global state a scheduled job cannot reason about, while
// the user switches accounts by hand in parallel.
//
// Target-repo commands must NOT get this. See SandboxEnv.
func Env(id Identity, token string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+7)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GH_TOKEN", "GITHUB_TOKEN": // would race with the value we set
		default:
			out = append(out, kv)
		}
	}
	return append(out,
		"GH_TOKEN="+token,
		"GIT_AUTHOR_NAME="+id.Name,
		"GIT_AUTHOR_EMAIL="+id.Email,
		"GIT_COMMITTER_NAME="+id.Name,
		"GIT_COMMITTER_EMAIL="+id.Email,
		"GIT_TERMINAL_PROMPT=0",
	)
}

// SandboxEnv is the environment for running a TARGET repo's own commands.
//
// `go test`, `npm test` and friends execute code the other project controls.
// They have no business seeing our token, so every credential-shaped variable
// is stripped -- not just ours. In v2 these run in a container as well; this
// is the belt to that braces.
func SandboxEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if looksSecret(k) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0")
}

func looksSecret(k string) bool {
	u := strings.ToUpper(k)
	switch u {
	case "GH_TOKEN", "GITHUB_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN":
		return true
	}
	for _, suffix := range []string{"_TOKEN", "_SECRET", "_KEY", "_PASSWORD", "_CREDENTIALS"} {
		if strings.HasSuffix(u, suffix) {
			return true
		}
	}
	return strings.HasPrefix(u, "AWS_")
}

func git(clone string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", clone}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// HardenClone applies layers 2-4 to a fresh clone. Must run before any commit.
func HardenClone(clone string, id Identity) error {
	steps := [][]string{
		{"config", "--local", "user.name", id.Name},
		{"config", "--local", "user.email", id.Email},
		// The empty value resets the helper chain, discarding the global
		// helper that would otherwise serve the other account's token. Order
		// matters: git reads system -> global -> local, so a local reset
		// clears everything ahead of it.
		{"config", "--local", "--replace-all", CredentialKey, ""},
		{"config", "--local", "--add", CredentialKey, id.HelperPath()},
		// One canonical hooks directory rather than per-clone copies: no
		// drift, and an untrusted repo's own hooks never execute.
		{"config", "--local", "core.hooksPath", id.HooksPath()},
	}
	for _, s := range steps {
		if _, err := git(clone, s...); err != nil {
			return fmt.Errorf("harden %s: git %s: %w", clone, strings.Join(s, " "), err)
		}
	}
	return ExcludeAgentScaffolding(clone)
}

// agentScaffolding is what an implementer may leave behind in a clone.
//
// Several target repositories ship their own `.claude/skills/...`, and an
// auto-fix run has twice produced a copy under `.agents/` -- which `git add -A`
// would then commit into a public pull request. The submit guard catches that,
// but catching is weaker than preventing.
var agentScaffolding = []string{".agents/", ".aider*", ".cursor/", ".codex/"}

// ExcludeAgentScaffolding makes agent leftovers unstageable in this clone.
//
// Written to .git/info/exclude rather than .gitignore: it is our local
// concern, and editing a repository's tracked .gitignore would appear in the
// diff we are about to ask someone to merge.
func ExcludeAgentScaffolding(clone string) error {
	path := filepath.Join(clone, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	existing, _ := os.ReadFile(path)
	var missing []string
	for _, p := range agentScaffolding {
		if !strings.Contains(string(existing), p) {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	out := string(existing)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += "# added by oss-pipeline: never stage agent scaffolding\n" +
		strings.Join(missing, "\n") + "\n"
	return os.WriteFile(path, []byte(out), 0o644)
}

// EffectiveCredentialHelpers is the chain git will actually consult.
//
// An empty entry clears every helper declared before it, so only the entries
// after the last empty one are live. Reading the raw list without applying
// that rule is how a reset can look like a leak, or worse, the reverse.
func EffectiveCredentialHelpers(clone string) []string {
	out, _ := git(clone, "config", "--get-all", CredentialKey)
	if out == "" {
		return nil
	}
	vals := strings.Split(out, "\n")
	last := -1
	for i, v := range vals {
		if strings.TrimSpace(v) == "" {
			last = i
		}
	}
	return vals[last+1:]
}

// AssertClone is the pre-flight check before every commit and every push.
//
// Independent of the git hooks by design: this is what still holds when a
// repo's tooling has rewritten core.hooksPath out from under us.
func AssertClone(clone string, id Identity) error {
	var problems []string

	name, _ := git(clone, "config", "--local", "user.name")
	email, _ := git(clone, "config", "--local", "user.email")
	if email != id.Email {
		hint := ""
		for i, login := range id.OtherLogins {
			if i < len(id.OtherEmails) && email == id.OtherEmails[i] {
				which := "another of your"
				if login == id.WorkLogin {
					which = "the WORK"
				}
				hint = fmt.Sprintf(" (this is %s account %s)", which, login)
				break
			}
		}
		problems = append(problems,
			fmt.Sprintf("local user.email is %q, expected %q%s", email, id.Email, hint))
	}
	if name != id.Name {
		problems = append(problems,
			fmt.Sprintf("local user.name is %q, expected %q", name, id.Name))
	}

	helpers := EffectiveCredentialHelpers(clone)
	if len(helpers) != 1 || helpers[0] != id.HelperPath() {
		problems = append(problems, fmt.Sprintf(
			"credential helper chain is %q, expected [%q] -- another account's "+
				"token could be served to a push", helpers, id.HelperPath()))
	}

	hooks, _ := git(clone, "config", "--local", "core.hooksPath")
	if hooks != id.HooksPath() {
		problems = append(problems, fmt.Sprintf(
			"core.hooksPath is %q, expected %q -- layer 4 guards are disarmed "+
				"(did a package manager rewrite it?)", hooks, id.HooksPath()))
	}

	// The `fork` remote is the only one this pipeline ever pushes to, so it is
	// the only one whose URL can put a branch on the wrong account. A clone
	// reused from an earlier run, or one whose remote was rewritten by a
	// script in the repository, is caught here rather than by a maintainer
	// seeing a push from a name they can link to the user.
	if url, err := git(clone, "remote", "get-url", "--push", "fork"); err == nil {
		if !ownedBy(url, id.Login) {
			problems = append(problems, fmt.Sprintf(
				"remote \"fork\" pushes to %q, which is not a %s repository",
				url, id.Login))
		}
	}

	if len(problems) > 0 {
		return &Error{Problems: problems}
	}
	return nil
}

// IsIdentityError reports whether err came from this package's assertions.
func IsIdentityError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

// PrivateStrings are the literals that must never reach a public diff or pull
// request body: the user's other accounts and addresses.
//
// Returned from the loaded identity rather than written into the scanner,
// because the scanner lives in a repository that is public. Hardcoding the
// address the check exists to suppress would publish it the moment anyone read
// the source.
func (i Identity) PrivateStrings() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range append(append([]string{i.WorkEmail, i.WorkLogin},
		i.OtherEmails...), i.OtherLogins...) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ownedBy reports whether a git remote URL names a repository under login.
// Both forms GitHub hands out are accepted: https://github.com/login/name and
// git@github.com:login/name.
func ownedBy(url, login string) bool {
	return strings.Contains(url, "/"+login+"/") || strings.Contains(url, ":"+login+"/")
}
