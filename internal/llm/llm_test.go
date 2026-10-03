package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func stub(out string, err error) *Client {
	c := New(nil)
	c.exec = func(ctx context.Context, dir string, args []string, stdin string, env []string) (string, string, error) {
		return out, "", err
	}
	return c
}

// TestJudgeDisablesEveryTool is a security test, not a behaviour test. Judge
// is routinely handed a third-party issue thread; anything in that text which
// reads like an instruction must not be able to reach a file or a shell.
func TestJudgeDisablesEveryTool(t *testing.T) {
	var got []string
	c := New(nil)
	c.exec = func(ctx context.Context, dir string, args []string, stdin string, env []string) (string, string, error) {
		got = args
		return "ok", "", nil
	}
	if _, err := c.Judge(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--disallowed-tools") {
		t.Fatal("Judge must disable tools")
	}
	for _, tool := range []string{"Read", "Write", "Edit", "Bash"} {
		if !strings.Contains(joined, tool) {
			t.Errorf("%s is not disabled: %v", tool, got)
		}
	}
	if strings.Contains(joined, "acceptEdits") || strings.Contains(joined, "bypassPermissions") {
		t.Error("Judge must never run with edit permissions")
	}
}

func TestExtractJSON(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		wantErr        bool
	}{
		{"bare object", `{"a":1}`, `{"a":1}`, false},
		{"with prose", "Here you go:\n{\"a\":1}\nHope that helps", `{"a":1}`, false},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`, false},
		{"array", `[{"a":1},{"b":2}]`, `[{"a":1},{"b":2}]`, false},
		{"nested", `{"a":{"b":[1,2]}}`, `{"a":{"b":[1,2]}}`, false},
		// A brace inside a string must not close the object -- issue threads
		// are full of code samples containing braces.
		{"brace in string", `{"note":"use } carefully"}`, `{"note":"use } carefully"}`, false},
		{"escaped quote", `{"note":"say \"hi\" {"}`, `{"note":"say \"hi\" {"}`, false},
		{"no json", "I could not determine that", "", true},
		{"unterminated", `{"a":1`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractJSON(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestJudgeJSONDecodes(t *testing.T) {
	var v struct {
		Class string `json:"class"`
	}
	err := stub("```json\n{\"class\":\"mechanical\"}\n```", nil).
		JudgeJSON(context.Background(), "p", &v)
	if err != nil {
		t.Fatal(err)
	}
	if v.Class != "mechanical" {
		t.Fatalf("class = %q", v.Class)
	}
}

// An unusable answer gets one more try with an explicit complaint, because a
// failed judgement blocks a whole stage.
func TestJudgeJSONRetriesOnce(t *testing.T) {
	calls := 0
	c := New(nil)
	c.exec = func(ctx context.Context, dir string, args []string, stdin string, env []string) (string, string, error) {
		calls++
		if calls == 1 {
			return "I'm not sure what you mean.", "", nil
		}
		return `{"class":"informational"}`, "", nil
	}
	var v struct {
		Class string `json:"class"`
	}
	if err := c.JudgeJSON(context.Background(), "p", &v); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if v.Class != "informational" {
		t.Fatalf("class = %q", v.Class)
	}
}

func TestJudgeJSONGivesUpAfterTwo(t *testing.T) {
	var v map[string]any
	err := stub("still no json", nil).JudgeJSON(context.Background(), "p", &v)
	if !errors.Is(err, ErrLLM) {
		t.Fatalf("want ErrLLM, got %v", err)
	}
}

func TestRender(t *testing.T) {
	got := Render("Fix {{repo}}#{{issue}}", map[string]string{
		"repo": "a/b", "issue": "7",
	})
	if got != "Fix a/b#7" {
		t.Fatalf("got %q", got)
	}
}

// TestPatchHasNoShell is the security boundary of the v2 design, asserted
// against the argv rather than against a comment.
//
// The patch agent may read and write files in the clone and nothing else. With
// Bash it could run the repository's build, its tests, or whatever a malicious
// postinstall left behind -- on the host, with the user's home directory one
// path away. Every command instead runs afterwards in a container, driven by
// the pipeline, which is what makes "no target-repo code executes on the host"
// a property of the process rather than a rule an agent is asked to respect.
func TestPatchHasNoShell(t *testing.T) {
	var got []string
	var dir string
	c := New(nil)
	c.exec = func(_ context.Context, d string, args []string, _ string, _ []string) (string, string, error) {
		got, dir = args, d
		return "done", "", nil
	}
	if _, err := c.Patch(context.Background(), "/clones/kornia", "write the patch"); err != nil {
		t.Fatal(err)
	}

	disallowed := map[string]bool{}
	for i, a := range got {
		if a == "--disallowed-tools" {
			for _, rest := range got[i+1:] {
				if strings.HasPrefix(rest, "-") {
					break
				}
				disallowed[rest] = true
			}
		}
	}
	for _, must := range []string{"Bash", "WebFetch", "WebSearch"} {
		if !disallowed[must] {
			t.Errorf("%s is not disallowed; argv = %v", must, got)
		}
	}
	// Edits must still be possible, or the agent cannot write the patch at
	// all and the whole stage is a no-op that looks like a refusal.
	for _, mustNot := range []string{"Read", "Write", "Edit"} {
		if disallowed[mustNot] {
			t.Errorf("%s is disallowed, so no patch can be written", mustNot)
		}
	}
	if dir != "/clones/kornia" {
		t.Errorf("agent ran in %q, want the clone", dir)
	}
}

// TestJudgeCallsCannotReachTheFilesystem: a judgement is routinely handed a
// third-party issue thread or diff, which is untrusted text. Anything in it
// that reads like an instruction must not be able to reach a file or a shell.
func TestJudgeCallsCannotReachTheFilesystem(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Client) error
	}{
		{"Judge", func(c *Client) error { _, err := c.Judge(context.Background(), "p"); return err }},
		{"JudgeWith", func(c *Client) error {
			_, err := c.JudgeWith(context.Background(), "p", "s", "untrusted thread")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			c := New(nil)
			c.exec = func(_ context.Context, _ string, args []string, _ string, _ []string) (string, string, error) {
				got = args
				return "{}", "", nil
			}
			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(got, " ")
			for _, must := range []string{"Read", "Write", "Edit", "Bash", "WebFetch", "WebSearch"} {
				if !strings.Contains(joined, must) {
					t.Errorf("%s is not disallowed; argv = %v", must, got)
				}
			}
		})
	}
}

// TestNoInvocationInheritsTheUsersMCPServers is the fix for the hole that
// mattered most in this whole session.
//
// The design says the patch agent may Read, Write and Edit files in the clone
// and nothing else -- no shell, so it cannot execute a target repository's
// code. MCP servers walked straight through that: the CLI loads whatever the
// user has configured globally, so an agent working inside a third-party
// clone had browser automation and a filesystem server as well. It was only
// noticed because one of those servers wrote a .serena/ directory into
// kubernetes-sigs/kind and preflight refused to ship it.
//
// Every invocation, not just the patch one. A judgement call that can reach a
// browser is worse than a patch that can.
func TestNoInvocationInheritsTheUsersMCPServers(t *testing.T) {
	calls := map[string][]string{}
	record := func(name string) func(context.Context, string, []string, string, []string) (string, string, error) {
		return func(_ context.Context, _ string, args []string, _ string, _ []string) (string, string, error) {
			calls[name] = args
			return "ok", "", nil
		}
	}

	c := New(nil)
	c.exec = record("Judge")
	_, _ = c.Judge(context.Background(), "p")
	c.exec = record("JudgeWith")
	_, _ = c.JudgeWith(context.Background(), "p", "s", "in")
	c.exec = record("Patch")
	_, _ = c.Patch(context.Background(), t.TempDir(), "p")

	if len(calls) != 3 {
		t.Fatalf("expected three invocations, captured %d: %v", len(calls), calls)
	}
	for name, args := range calls {
		joined := strings.Join(args, " ")
		if !contains(args, "--strict-mcp-config") {
			t.Errorf("%s does not pass --strict-mcp-config; argv = %v", name, args)
		}
		// With the flag and no --mcp-config, the set of servers is empty. A
		// --mcp-config appearing here would be granting tools back.
		if strings.Contains(joined, "--mcp-config") {
			t.Errorf("%s loads an MCP config: %v", name, args)
		}
	}
	// The patch call also drops the clone's own settings: a repository we are
	// about to run an agent inside must not get to configure that agent.
	if !contains(calls["Patch"], "--restricted") {
		t.Errorf("Patch does not pass --restricted; argv = %v", calls["Patch"])
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
