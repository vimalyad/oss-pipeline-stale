package identity

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeEnv builds an identity.env inside a fake pipeline root.
func writeEnv(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "config"), 0o755)
	p := filepath.Join(root, "config", "identity.env")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const sample = `
# The account everything public happens under.
OSS_LOGIN="someone"
OSS_NAME="Some One"
# A comment that mentions OSS_EMAIL, so these are not a security boundary --
# they exist so a blocked push explains itself.
OSS_EMAIL="1234+someone@users.noreply.github.com"
OSS_KEYCHAIN_ACCOUNT="someone"
OSS_KEYCHAIN_SERVICE="svc"
WORK_LOGIN="workacct"
WORK_EMAIL="99+workacct@users.noreply.github.com"
OTHER_LOGINS="workacct thirdacct"
OTHER_EMAILS="99+workacct@users.noreply.github.com 77+thirdacct@users.noreply.github.com"
`

// TestParseIgnoresCommentsMentioningKeys is a regression test for a real
// incident: a shell `grep OSS_EMAIL identity.env | cut -d'"' -f2` matched the
// *comment* line above as well as the value, and produced a commit whose
// author email had a sentence appended to it. One parser, and it must not be
// fooled by a key name appearing in prose.
func TestParseIgnoresCommentsMentioningKeys(t *testing.T) {
	id, err := Load(writeEnv(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "1234+someone@users.noreply.github.com" {
		t.Fatalf("email = %q, want the bare address", id.Email)
	}
	if strings.ContainsAny(id.Email, " #") {
		t.Fatalf("email %q carries comment text", id.Email)
	}
	if id.Name != "Some One" {
		t.Fatalf("name = %q", id.Name)
	}
	if len(id.OtherLogins) != 2 || id.OtherLogins[0] != "workacct" {
		t.Fatalf("other logins = %v", id.OtherLogins)
	}
}

func TestLoadReportsMissingKeys(t *testing.T) {
	_, err := Load(writeEnv(t, `OSS_LOGIN="x"`))
	if err == nil {
		t.Fatal("want an error naming the missing keys")
	}
	for _, want := range []string{"OSS_NAME", "OSS_EMAIL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s: %v", want, err)
		}
	}
}

// TestSandboxEnvCarriesNoCredentials is the whole point of having two env
// builders. A target repo's test command runs code that project controls.
func TestSandboxEnvCarriesNoCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_sentinel_value_must_not_leak")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-sentinel")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-sentinel")
	t.Setenv("MY_SERVICE_TOKEN", "other-sentinel")
	t.Setenv("PATH", "/usr/bin") // ordinary vars must survive

	got := strings.Join(SandboxEnv(), "\n")
	for _, sentinel := range []string{
		"ghp_sentinel_value_must_not_leak", "sk-ant-sentinel",
		"aws-sentinel", "other-sentinel",
	} {
		if strings.Contains(got, sentinel) {
			t.Errorf("SandboxEnv leaked %q", sentinel)
		}
	}
	if !strings.Contains(got, "PATH=/usr/bin") {
		t.Error("SandboxEnv dropped PATH; the command will not run")
	}
}

func TestEnvCarriesTheTokenAndIdentity(t *testing.T) {
	id := Identity{Name: "Some One", Email: "e@x", Root: "/r"}
	got := strings.Join(Env(id, "tok"), "\n")
	for _, want := range []string{
		"GH_TOKEN=tok", "GIT_AUTHOR_EMAIL=e@x", "GIT_COMMITTER_NAME=Some One",
		"GIT_TERMINAL_PROMPT=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Env missing %s", want)
		}
	}
}

func TestEnvDoesNotInheritAConflictingToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "stale-would-race")
	got := strings.Join(Env(Identity{}, "tok"), "\n")
	if strings.Contains(got, "stale-would-race") {
		t.Error("GITHUB_TOKEN would race with GH_TOKEN and must be dropped")
	}
}

func TestEffectiveCredentialHelpersAppliesResetSemantics(t *testing.T) {
	clone := initRepo(t)
	run(t, clone, "config", "--local", "--add", CredentialKey, "!inherited-helper")
	run(t, clone, "config", "--local", "--add", CredentialKey, "")
	run(t, clone, "config", "--local", "--add", CredentialKey, "/ours")

	got := EffectiveCredentialHelpers(clone)
	if len(got) != 1 || got[0] != "/ours" {
		t.Fatalf("chain = %v, want only the helper after the reset", got)
	}
}

// --- the throwaway-clone verification the plan requires -------------------

func initRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	run(t, d, "init", "-q")
	return d
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func testIdentity(t *testing.T) Identity {
	t.Helper()
	id, err := Load(writeEnv(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHardenThenAssertPasses(t *testing.T) {
	clone, id := initRepo(t), testIdentity(t)
	if err := HardenClone(clone, id); err != nil {
		t.Fatal(err)
	}
	if err := AssertClone(clone, id); err != nil {
		t.Fatalf("a freshly hardened clone must pass: %v", err)
	}
}

// TestAssertCatchesEachLayerIndependently breaks one layer at a time. Each
// must be caught on its own, because in production they fail on their own:
// npm install rewrites hooksPath without touching user.email.
func TestAssertCatchesEachLayerIndependently(t *testing.T) {
	cases := []struct {
		name, wantSubstring string
		breakIt             func(t *testing.T, clone string, id Identity)
	}{
		{"wrong commit email", "expected", func(t *testing.T, c string, id Identity) {
			run(t, c, "config", "--local", "user.email", "wrong@example.com")
		}},
		{"the work account's email", "WORK account", func(t *testing.T, c string, id Identity) {
			run(t, c, "config", "--local", "user.email", id.WorkEmail)
		}},
		{"hooks disarmed", "disarmed", func(t *testing.T, c string, id Identity) {
			run(t, c, "config", "--local", "core.hooksPath", ".husky")
		}},
		{"inherited credential helper", "token could be served",
			func(t *testing.T, c string, id Identity) {
				run(t, c, "config", "--local", "--add", CredentialKey, "!gh auth git-credential")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clone, id := initRepo(t), testIdentity(t)
			if err := HardenClone(clone, id); err != nil {
				t.Fatal(err)
			}
			tc.breakIt(t, clone, id)
			err := AssertClone(clone, id)
			if err == nil {
				t.Fatal("guard did not fire")
			}
			if !IsIdentityError(err) {
				t.Fatalf("want an identity error, got %T", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Errorf("message should explain the problem; got: %v", err)
			}
		})
	}
}

// TestAssertReportsEveryProblemAtOnce: an unattended failure is only
// debuggable if it shows the whole picture, not the first thing it hit.
func TestAssertReportsEveryProblemAtOnce(t *testing.T) {
	clone, id := initRepo(t), testIdentity(t)
	if err := HardenClone(clone, id); err != nil {
		t.Fatal(err)
	}
	run(t, clone, "config", "--local", "user.email", "wrong@example.com")
	run(t, clone, "config", "--local", "user.name", "Someone Else")
	run(t, clone, "config", "--local", "core.hooksPath", ".husky")

	var e *Error
	err := AssertClone(clone, id)
	if !IsIdentityError(err) {
		t.Fatalf("want identity error, got %v", err)
	}
	e, _ = err.(*Error)
	if len(e.Problems) < 3 {
		t.Fatalf("want all 3 problems, got %d: %v", len(e.Problems), e.Problems)
	}
}

// TestPrivateStringsGathersEveryAccount. These are the literals the secret
// scanner refuses to let into a public diff, and a missing one is a leak.
func TestPrivateStrings(t *testing.T) {
	id := Identity{
		Login: "vimalyad", Name: "A Name", Email: "1+vimalyad@users.noreply.github.com",
		WorkLogin: "workacct", WorkEmail: "a@employer.example",
		OtherLogins: []string{"otheracct", "workacct"},
		OtherEmails: []string{"b@example.com", "", "  "},
	}
	got := id.PrivateStrings()

	want := map[string]bool{"workacct": true, "a@employer.example": true,
		"otheracct": true, "b@example.com": true}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected private string %q", s)
		}
		delete(want, s)
	}
	for s := range want {
		t.Errorf("missing private string %q", s)
	}
	// The publishing identity itself is not private -- it is the whole point
	// of the account, and listing it would block every commit it authors.
	for _, s := range got {
		if s == id.Login || s == id.Email {
			t.Errorf("the publishing identity %q was treated as private", s)
		}
	}
	// Blank entries and duplicates must not become scanner patterns: an empty
	// string matches every diff.
	for _, s := range got {
		if strings.TrimSpace(s) == "" {
			t.Error("a blank private string would match every diff")
		}
	}
	if len(got) != 4 {
		t.Errorf("got %d strings, want 4 after deduplication: %v", len(got), got)
	}
}

func TestPrivateStringsOnAnEmptyIdentity(t *testing.T) {
	if got := (Identity{}).PrivateStrings(); len(got) != 0 {
		t.Fatalf("= %v, want nothing", got)
	}
}

// TestAssertCloneRejectsAForkRemoteOnAnotherAccount is the check that stands
// between a verified patch and a push landing under the user's real name. The
// three other layers all guard the commit; nothing before this guarded where
// the commit goes.
func TestAssertCloneRejectsAForkRemoteOnAnotherAccount(t *testing.T) {
	clone, id := initRepo(t), testIdentity(t)
	if err := HardenClone(clone, id); err != nil {
		t.Fatal(err)
	}
	if err := AssertClone(clone, id); err != nil {
		t.Fatalf("a clone with no fork remote should pass: %v", err)
	}

	for _, url := range []string{
		"https://github.com/someone-else/helm.git",
		"git@github.com:someone-else/helm.git",
	} {
		run(t, clone, "remote", "add", "fork", url)
		err := AssertClone(clone, id)
		if err == nil {
			t.Fatalf("%s was accepted", url)
		}
		if !strings.Contains(err.Error(), "not a "+id.Login+" repository") {
			t.Fatalf("unhelpful message for %s: %v", url, err)
		}
		run(t, clone, "remote", "remove", "fork")
	}

	run(t, clone, "remote", "add", "fork", "https://github.com/"+id.Login+"/helm.git")
	if err := AssertClone(clone, id); err != nil {
		t.Fatalf("our own fork was rejected: %v", err)
	}
}
