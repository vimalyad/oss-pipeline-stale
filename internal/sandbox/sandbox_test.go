package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func spec() Spec {
	return Spec{
		Image: "ossp-gate:test", Clone: "/host/clone",
		Volumes: map[string]string{"ossp-cache-go": "/cache"},
		Limits:  DefaultLimits(),
	}
}

func flagString(s Spec) string { return strings.Join(dockerFlags(s), " ") }

// flagValue returns the argument that follows name, so a test can assert on
// the value itself rather than on a substring of the whole command line.
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestFlagsCarryNoCredential is the security property this package exists for.
// The host environment is never passed through, so a credential in the shell
// cannot reach a target repository's build.
func TestFlagsCarryNoCredential(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_sentinel_must_not_appear")
	t.Setenv("OPENAI_API_KEY", "sk-sentinel-must-not-appear")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-sentinel")

	got := flagString(spec())
	for _, sentinel := range []string{
		"ghp_sentinel_must_not_appear", "sk-sentinel-must-not-appear", "sk-ant-sentinel",
	} {
		if strings.Contains(got, sentinel) {
			t.Fatalf("a host credential reached the container flags: %q", sentinel)
		}
	}
	if strings.Contains(got, "--env-file") || strings.Contains(got, "-e ") {
		t.Errorf("no bulk environment passthrough is permitted: %s", got)
	}
}

// Only variables the caller names explicitly are passed.
func TestOnlyExplicitEnvIsPassed(t *testing.T) {
	s := spec()
	s.Env = map[string]string{"CI": "1", "GOFLAGS": "-mod=mod"}
	got := flagString(s)
	if !strings.Contains(got, "--env CI=1") || !strings.Contains(got, "--env GOFLAGS=-mod=mod") {
		t.Fatalf("explicit env missing: %s", got)
	}
}

func TestNetworkIsOffByDefault(t *testing.T) {
	if !strings.Contains(flagString(spec()), "--network none") {
		t.Fatal("verification must run without network; it is the oracle")
	}
	s := spec()
	s.Network = true
	if strings.Contains(flagString(s), "--network none") {
		t.Fatal("an install step that asked for network did not get it")
	}
}

func TestHardeningFlagsArePresent(t *testing.T) {
	got := flagString(spec())
	for _, want := range []string{
		"--user 1000:1000", "--cap-drop ALL", "--security-opt no-new-privileges",
		"--pids-limit", "--memory 8g", "--memory-swap 8g", "--cpus 6",
		"--label ossp.managed=1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// /tmp must be nosuid but NOT noexec: Go and cargo link test binaries into
// /tmp and execute them from there.
//
// The flag assertion alone is not enough, and this test used to make only that
// assertion. Docker applies noexec to every --tmpfs by default no matter what
// options are passed, so a flag string that merely lacks the word "noexec"
// still produces a noexec mount. The unit test passed while every `go test`
// in the sandbox failed with "permission denied". TestTmpfsActuallyExecutes
// below is the one that can tell.
func TestTmpfsFlagRequestsExec(t *testing.T) {
	got := flagValue(dockerFlags(Spec{Limits: DefaultLimits()}), "--tmpfs")
	if !strings.HasPrefix(got, "/tmp:") || !strings.Contains(got, "nosuid") {
		t.Fatalf("tmpfs flag wrong: %s", got)
	}
	if !strings.Contains(got, ",exec") {
		t.Fatal("docker defaults --tmpfs to noexec; `exec` must be requested explicitly")
	}
	if strings.Contains(got, "noexec") {
		t.Fatal("noexec on /tmp breaks go and cargo test binaries")
	}
}

func TestEverythingIsLabelled(t *testing.T) {
	if !strings.Contains(flagString(spec()), "--label ossp.managed=1") {
		t.Fatal("unlabelled objects cannot be pruned safely")
	}
}

// Go randomises map iteration; without sorting, the same spec produces
// different flags run to run and these tests become flaky.
func TestFlagOrderIsDeterministic(t *testing.T) {
	s := spec()
	s.Volumes = map[string]string{"a": "/a", "b": "/b", "c": "/c", "d": "/d"}
	s.Env = map[string]string{"X": "1", "Y": "2", "Z": "3"}
	first := flagString(s)
	for i := 0; i < 20; i++ {
		if got := flagString(s); got != first {
			t.Fatalf("flags differ between calls:\n%s\n%s", first, got)
		}
	}
}

func TestResultOK(t *testing.T) {
	if !(Result{Code: 0}).OK() {
		t.Error("exit 0 is success")
	}
	if (Result{Code: 0, TimedOut: true}).OK() {
		t.Error("a timeout is not success even at exit 0")
	}
}

func TestTailBoundsOutput(t *testing.T) {
	long := strings.Repeat("x", 10000)
	got := tail(long, 100)
	if len(got) > 200 {
		t.Fatalf("tail did not bound output: %d bytes", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation should be visible in the output")
	}
}

// --- integration: only with -tags docker ---------------------------------

func dockerAvailable(t *testing.T) {
	t.Helper()
	if os.Getenv("OSSP_DOCKER_TESTS") == "" {
		t.Skip("set OSSP_DOCKER_TESTS=1 to run container integration tests")
	}
	if ok, why := Available(context.Background()); !ok {
		t.Skipf("docker not available: %s", why)
	}
}

func TestIntegrationCredentialIsolation(t *testing.T) {
	dockerAvailable(t)
	t.Setenv("GH_TOKEN", "ghp_sentinel_isolation_check_xxxxxxxx")
	dir := t.TempDir()

	s, err := Start(context.Background(), Spec{
		Image: "ossp-gate:test", Clone: dir, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	res, err := s.Run(context.Background(), "env; cat /proc/self/environ | tr '\\0' '\\n'")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "sentinel_isolation_check") {
		t.Fatal("a host credential was visible inside the container")
	}
	insp, _ := exec.Command("docker", "inspect", s.ID, "--format", "{{json .Config.Env}}").Output()
	if strings.Contains(string(insp), "sentinel_isolation_check") {
		t.Fatal("the credential is visible in docker inspect")
	}
}

func TestIntegrationOwnershipRoundTrip(t *testing.T) {
	dockerAvailable(t)
	dir := t.TempDir()
	s, err := Start(context.Background(), Spec{
		Image: "ossp-gate:test", Clone: dir, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Run(context.Background(), "touch /work/canary"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "canary"))
	if err != nil {
		t.Fatalf("the file did not reach the host: %v", err)
	}
	if !st.Mode().IsRegular() {
		t.Fatal("unexpected file type")
	}
}

func TestIntegrationNetworkIsBlocked(t *testing.T) {
	dockerAvailable(t)
	s, err := Start(context.Background(), Spec{
		Image: "ossp-gate:test", Clone: t.TempDir(), Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	res, _ := s.Run(context.Background(), "getent hosts api.github.com")
	if res.Code == 0 {
		t.Fatal("DNS resolved inside a --network none container")
	}
}

func TestIntegrationTimeoutKillsTheProcess(t *testing.T) {
	dockerAvailable(t)
	lim := DefaultLimits()
	lim.Timeout = 3 * time.Second
	s, err := Start(context.Background(), Spec{
		Image: "ossp-gate:test", Clone: t.TempDir(), Limits: lim,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	res, err := s.Run(context.Background(), "sleep 60")
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatal("a command past its timeout must be reported as timed out")
	}
}

// TestIntegrationTmpfsActuallyExecutes is the test that can tell, and the one
// that was missing.
//
// The unit test above only reads the flag we pass. Docker applies noexec to
// every --tmpfs by default no matter what options are given, so a flag string
// that lacks the word "noexec" still produced a noexec /tmp. This compiles a
// binary into /tmp and runs it, which is exactly what `go test` does for every
// package, and is the only way to observe the difference from outside.
func TestIntegrationTmpfsActuallyExecutes(t *testing.T) {
	dockerAvailable(t)
	s, err := Start(context.Background(), Spec{
		Image: "ossp-gate:test", Clone: t.TempDir(), Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	res, err := s.Run(context.Background(),
		`printf '#!/bin/sh\necho ran-from-tmp\n' > /tmp/probe && chmod +x /tmp/probe && /tmp/probe`)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || !strings.Contains(res.Output, "ran-from-tmp") {
		t.Fatalf("could not execute from /tmp (code=%d): %s\n"+
			"a noexec /tmp makes every go and cargo test fail with "+
			"\"permission denied\", which reads like a broken patch", res.Code, res.Output)
	}
}

// TestSanitiseGitConfigRemovesHostPaths covers the clone config the pipeline
// actually writes. The credential helper is the reason this exists: the
// container's premise is that it holds no credential, and a config that names
// the host script which dispenses one contradicts that.
func TestSanitiseGitConfigRemovesHostPaths(t *testing.T) {
	clone := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Verbatim shape of a clone this pipeline has prepared.
	cfg := `[core]
	repositoryformatversion = 0
	hooksPath = /Users/someone/oss-pipeline/githooks
[remote "origin"]
	url = https://github.com/cli/cli.git
[credential "https://github.com"]
	helper =
	helper = /Users/someone/oss-pipeline/bin/gh-token-helper
[user]
	name = A Name
	email = 1234+login@users.noreply.github.com
`
	if err := os.WriteFile(filepath.Join(clone, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sanitiseGitConfig(clone)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, forbidden := range []string{"gh-token-helper", "credential", "hooksPath", "githooks"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("sanitised config still contains %q:\n%s", forbidden, got)
		}
	}
	// The section after the stripped one must survive: a naive strip that
	// runs to end-of-file would silently drop the remote and the identity.
	for _, want := range []string{"[user]", "noreply.github.com", "[remote \"origin\"]", "cli/cli.git"} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitised config lost %q:\n%s", want, got)
		}
	}
	if filepath.Dir(out) != filepath.Join(clone, ".git") {
		t.Errorf("sanitised config written outside the clone's .git: %s", out)
	}
}

func TestSanitiseGitConfigOnANonClone(t *testing.T) {
	// A plain directory, and a .git file rather than a directory (a worktree
	// or submodule), must both be a no-op rather than an error.
	p, err := sanitiseGitConfig(t.TempDir())
	if err != nil || p != "" {
		t.Fatalf("plain dir: %q, %v", p, err)
	}
	clone := t.TempDir()
	if err := os.WriteFile(filepath.Join(clone, ".git"), []byte("gitdir: ../x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, err := sanitiseGitConfig(clone); err != nil || p != "" {
		t.Fatalf("gitfile: %q, %v", p, err)
	}
}

// TestIntegrationCloneConfigHasNoHostPathsInside is the end-to-end form of the
// test above, run against a real prepared clone if one is present. The unit
// test proves the filter; this proves the mount is actually wired and that
// git inside the container agrees.
func TestIntegrationCloneConfigHasNoHostPathsInside(t *testing.T) {
	dockerAvailable(t)
	clone := os.Getenv("OSSP_TEST_CLONE")
	if clone == "" {
		t.Skip("set OSSP_TEST_CLONE to a prepared clone")
	}
	s, err := Start(context.Background(), Spec{
		Image: "ossp-gate:test", Clone: clone, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	res, err := s.Run(context.Background(), "git config --list 2>/dev/null | grep -i 'credential\\|hookspath' || echo CLEAN")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "CLEAN") {
		t.Fatalf("container can see host git settings:\n%s", res.Output)
	}

	// And the sanitised file must not outlive the session.
	stray := filepath.Join(clone, ".git", sandboxConfigName)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("%s survived Close", stray)
	}
}

// TestIntegrationDetachRemovesEgress covers the install/verify split: a
// dependency install needs the network, the verification run must not have
// it, and a second container would lose the install because it lands in the
// writable layer rather than the clone.
func TestIntegrationDetachRemovesEgress(t *testing.T) {
	dockerAvailable(t)
	s, err := Start(context.Background(), Spec{
		Image: "ossp-gate:test", Clone: t.TempDir(), Network: true, Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if s.Networkless() {
		t.Fatal("a session started with Network:true reports itself networkless")
	}
	if res, err := s.Run(ctx, "getent hosts github.com"); err != nil || !res.OK() {
		t.Skipf("no egress available to test the removal of: %v", err)
	}
	// Something written before the detach must survive it: that persistence
	// is the entire reason for detaching rather than starting a new container.
	// $HOME is in the container's writable layer, which is where an
	// installed dependency set actually lands.
	if res, err := s.Run(ctx, "echo installed > \"$HOME/marker\""); err != nil || !res.OK() {
		t.Fatalf("setup write failed: %v %+v", err, res)
	}

	if err := s.Detach(ctx); err != nil {
		t.Fatal(err)
	}
	if !s.Networkless() {
		t.Error("Networkless() is false after Detach")
	}
	if res, _ := s.Run(ctx, "getent hosts github.com"); res.OK() {
		t.Error("DNS still resolves after Detach")
	}
	res, err := s.Run(ctx, "cat \"$HOME/marker\"")
	if err != nil || !strings.Contains(res.Output, "installed") {
		t.Errorf("the container lost its writable layer across Detach: %v %+v", err, res)
	}
}

// TestCommandsRunInANonLoginShell guards a one-character fix that cost a whole
// pipeline run to find.
//
// `bash -lc` sources /etc/profile, which on Debian resets PATH to a system
// default and discards everything the image set. `go` vanishes from a golang
// image and `cargo` from a rust one; node and python survive only because they
// install into /usr/local/bin, which the default PATH already covers. So the
// bug is invisible in the two ecosystems most likely to be tested with, and the
// symptom -- "go: command not found" -- classifies as an unbuildable
// repository rather than as a broken shell invocation.
func TestCommandsRunInANonLoginShell(t *testing.T) {
	args := execArgs("abc123")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/bin/bash -c") {
		t.Fatalf("exec args = %v, want a non-login shell", args)
	}
	for _, bad := range []string{"-lc", "-l", "--login"} {
		for _, a := range args {
			if a == bad {
				t.Fatalf("%q makes the shell discard the image's PATH", bad)
			}
		}
	}
	if args[len(args)-1] != "-c" {
		t.Errorf("the command must follow -c directly: %v", args)
	}
}

// TestIntegrationImageEnvironmentSurvives is the behavioural half: a language
// installed outside /usr/local/bin has to still be on PATH.
func TestIntegrationImageEnvironmentSurvives(t *testing.T) {
	dockerAvailable(t)
	if os.Getenv("OSSP_LANG_IMAGE") == "" {
		t.Skip("set OSSP_LANG_IMAGE to an image whose language lives outside /usr/local/bin")
	}
	s, err := Start(context.Background(), Spec{
		Image: os.Getenv("OSSP_LANG_IMAGE"), Clone: t.TempDir(), Limits: DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	probe := os.Getenv("OSSP_LANG_BINARY")
	if probe == "" {
		probe = "go"
	}
	res, err := s.Run(context.Background(), "command -v "+probe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("%s is not on PATH inside %s: the shell discarded the image's environment\n%s",
			probe, os.Getenv("OSSP_LANG_IMAGE"), res.Output)
	}
}
