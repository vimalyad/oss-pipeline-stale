// Package llm runs the `claude` CLI for the judgement steps.
//
// A subprocess rather than an SDK, for the same reason GitHub goes through
// `gh`: no API key plumbing on the host, and the CLI already handles auth,
// retries and model selection.
//
// Two kinds of call, and the difference is a security boundary:
//
//	Judge  -- reads text we pass in, with every tool disabled. It cannot read
//	          a file, run a command, or touch the network. Used for anything
//	          that looks at a third-party thread or diff.
//	Agent  -- writes code. Runs inside a container (see internal/sandbox),
//	          never on the host, because it executes the target repo's own
//	          build commands.
//
// Only Judge lives here. Agent belongs with the sandbox that confines it.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/text"
)

// ErrLLM is any failure to get a usable answer.
var ErrLLM = errors.New("llm")

// Model names. Judgement work uses the cheaper model; only code generation
// justifies the larger one.
const (
	ModelJudge = "sonnet"
	ModelCode  = "opus"
)

// DefaultTimeout is generous because these calls queue behind each other in an
// unattended run, and a killed call costs a whole stage.
const DefaultTimeout = 5 * time.Minute

type Client struct {
	// Env is deliberately NOT the identity environment: a judgement call has
	// no business holding a GitHub token.
	Env     []string
	Timeout time.Duration
	Log     func(string)
	// exec is swappable for tests. dir is the working directory, empty for
	// every call but Patch -- a judgement runs nowhere in particular, while
	// writing a patch happens inside the clone.
	exec func(ctx context.Context, dir string, args []string, stdin string, env []string) (string, string, error)
}

func New(env []string) *Client {
	return &Client{Env: env, Timeout: DefaultTimeout, exec: runClaude}
}

func (c *Client) logf(f string, a ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(f, a...))
	}
}

func runClaude(ctx context.Context, dir string, args []string, stdin string, env []string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = env
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// Judge asks a question about text, with every tool disabled.
//
// The tool restrictions are the point. This call is routinely handed a
// third-party issue thread or a diff, which is untrusted input: anything in it
// that reads like an instruction must not be able to reach a file or a shell.
func (c *Client) Judge(ctx context.Context, prompt string) (string, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--model", ModelJudge,
		"--disallowed-tools", "Read", "Write", "Edit", "Bash", "WebFetch", "WebSearch",
	}
	args = append(args, isolationFlags()...)
	out, errOut, err := c.exec(ctx, "", args, "", c.Env)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: timed out after %s", ErrLLM, timeout)
		}
		return "", fmt.Errorf("%w: %v: %s", ErrLLM, err, text.Ellipsis(errOut, 300))
	}
	return strings.TrimSpace(out), nil
}

// JudgeJSON runs Judge and decodes the answer into v.
//
// Models wrap JSON in prose or fences often enough that extracting it is the
// normal path, not error handling. One retry, because a second attempt with an
// explicit complaint usually succeeds and a failed judgement blocks a stage.
func (c *Client) JudgeJSON(ctx context.Context, prompt string, v any) error {
	attempt := func(p string) error {
		out, err := c.Judge(ctx, p)
		if err != nil {
			return err
		}
		blob, err := ExtractJSON(out)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(blob), v); err != nil {
			return fmt.Errorf("%w: response was not the expected shape: %v", ErrLLM, err)
		}
		return nil
	}
	err := attempt(prompt)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timed out") {
		return err
	}
	c.logf("    llm: retrying after unusable answer (%v)", err)
	return attempt(prompt + "\n\nReturn ONLY the JSON object. No prose, no code fences.")
}

// ExtractJSON pulls the first JSON object or array out of a model answer.
func ExtractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)
	if fenced := betweenFences(s); fenced != "" {
		s = fenced
	}
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return "", fmt.Errorf("%w: no JSON in answer: %s", ErrLLM, text.Ellipsis(s, 200))
	}
	open := s[start]
	close := byte('}')
	if open == '[' {
		close = ']'
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case esc:
			esc = false
		case ch == '\\' && inStr:
			esc = true
		case ch == '"':
			inStr = !inStr
		case inStr:
			// nothing: braces inside strings must not move the depth
		case ch == open:
			depth++
		case ch == close:
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("%w: unterminated JSON in answer: %s", ErrLLM, text.Ellipsis(s, 200))
}

func betweenFences(s string) string {
	i := strings.Index(s, "```")
	if i < 0 {
		return ""
	}
	rest := s[i+3:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	if j := strings.Index(rest, "```"); j >= 0 {
		return strings.TrimSpace(rest[:j])
	}
	return ""
}

// Render fills a prompt template. Deliberately minimal: {{key}} substitution
// with no logic, so a prompt stays readable as prose.
func Render(tmpl string, vars map[string]string) string {
	for k, v := range vars {
		tmpl = strings.ReplaceAll(tmpl, "{{"+k+"}}", v)
	}
	return tmpl
}

// JudgeWith is Judge with the payload on stdin and an extra system prompt.
//
// The payload goes on stdin rather than into the prompt for two reasons. An
// issue thread runs to tens of kilobytes and argv is not the place for it; and
// keeping the instructions and the untrusted text in separate channels makes
// the boundary between them explicit rather than a matter of formatting. The
// tools stay disabled for the same reason they are in Judge: the text is
// third-party, and anything in it that reads like an instruction must not be
// able to reach a file or a shell.
func (c *Client) JudgeWith(ctx context.Context, prompt, system, stdin string) (string, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--model", ModelJudge,
		"--disallowed-tools", "Read", "Write", "Edit", "Bash", "WebFetch", "WebSearch",
	}
	args = append(args, isolationFlags()...)
	if system != "" {
		args = append(args, "--append-system-prompt", system)
	}
	out, errOut, err := c.exec(ctx, "", args, stdin, c.Env)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: timed out after %s", ErrLLM, timeout)
		}
		return "", fmt.Errorf("%w: %v: %s", ErrLLM, err, text.Ellipsis(errOut, 300))
	}
	return strings.TrimSpace(out), nil
}

// PatchTimeout is generous: writing a patch means reading a codebase first.
const PatchTimeout = 60 * time.Minute

// Patch runs the coding agent inside a clone, with execution disabled.
//
// This is the one call that writes files, and the tool list is the security
// boundary of the whole v2 design. The agent may Read, Write and Edit inside
// the bind-mounted clone and nothing else: no Bash, so it cannot run the
// repository's build, its tests, or anything a malicious postinstall left
// behind. Every command the patch needs is run afterwards by the pipeline,
// inside a container, which is what makes "no target-repo code executes on the
// host" a property of the process rather than a rule the agent is asked to
// respect.
//
// It also means the audit trail is complete: each command run against a target
// repository is a decision of ours that was logged, not something an agent
// chose mid-turn.
func (c *Client) Patch(ctx context.Context, clone, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, PatchTimeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--model", ModelCode,
		"--permission-mode", "acceptEdits",
		"--add-dir", clone,
		// Bash is absent on purpose. See above.
		"--disallowed-tools", "Bash", "WebFetch", "WebSearch", "Task",
		// Confines the file tools to --add-dir and drops the clone's own
		// settings files. A target repository can carry a .claude/ directory,
		// and a repository we are about to run an agent inside must not get
		// to configure that agent.
		"--restricted",
	}
	args = append(args, isolationFlags()...)
	out, errOut, err := c.exec(ctx, clone, args, "", c.Env)
	if err != nil {
		if ctx.Err() != nil {
			return out, fmt.Errorf("%w: patch timed out after %s", ErrLLM, PatchTimeout)
		}
		return out, fmt.Errorf("%w: %v: %s", ErrLLM, err, text.Ellipsis(errOut, 300))
	}
	return strings.TrimSpace(out), nil
}

// isolationFlags keep the agent's tool surface to what this pipeline grants it.
//
// --strict-mcp-config with no --mcp-config means no MCP servers at all. Without
// it the CLI loads whatever the user has configured globally, and that is not a
// theoretical hole: a Serena server initialised a .serena/ project directory
// inside kubernetes-sigs/kind during a patch run, which preflight then refused
// to ship -- on every repository, forever. The directory was the visible half.
// The invisible half is that an agent working inside a third-party clone had
// browser automation, a filesystem server and everything else on the user's
// machine, while the design says it may Read, Write and Edit files in the clone
// and nothing else.
//
// The judgement calls get the same treatment. They need no tools at all, and a
// judgement call that could reach a browser is worse than a patch that can.
func isolationFlags() []string {
	return []string{"--strict-mcp-config"}
}
