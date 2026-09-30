package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func patterns(t *testing.T) *CommitPatterns {
	t.Helper()
	p, err := LoadCommitPatterns(filepath.Join("..", "..", "config", "forbidden-trailers.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommitPatternsBlockAttribution(t *testing.T) {
	p := patterns(t)
	for _, msg := range []string{
		"fix: handle broken symlinks\n\nFixes #13284\n\nCo-Authored-By: Claude <noreply@anthropic.com>",
		"fix: x\n\nco-authored-by: Cursor Agent <a@b.c>",
		"fix: x\n\nSigned-off-by: ChatGPT <a@b.c>",
		"fix: x\n\nGenerated with [Claude Code]",
		"fix: x\n\nWritten by an AI assistant.",
		"fix: x\n\nassisted by an llm.",
		"fix: x\n\nAI-generated patch.",
		"fix: x\n\n\U0001F916 beep",
	} {
		if got := p.Check(msg); len(got) == 0 {
			t.Errorf("allowed a message that must be blocked:\n%s", msg)
		}
	}
}

func TestCommitPatternsAllowOrdinaryMessages(t *testing.T) {
	p := patterns(t)
	for _, msg := range []string{
		// The real helm message this pipeline would write.
		"fix: .helmignore does not ignore symlinks\n\nFixes #13284",
		// Brand words are not attribution. A repo may legitimately have a
		// file called claude.py, and a commit may touch the Anthropic SDK.
		"feat(anthropic): add a retry to the client\n\nFixes #7",
		"docs: describe how the AI provider is configured",
		"fix: rename claude.py to provider.py",
		// A DCO sign-off under the user's own name is the project's rule,
		// not attribution, and must survive.
		"fix: x\n\nSigned-off-by: Vimal Yadav <1+vimalyad@users.noreply.github.com>",
	} {
		if got := p.Check(msg); len(got) > 0 {
			t.Errorf("blocked an ordinary message %q: %s", msg, strings.Join(got, "; "))
		}
	}
}

func TestCommitPatternsSayWhatMatched(t *testing.T) {
	p := patterns(t)
	got := p.Check("fix: x\n\nCo-Authored-By: Claude <noreply@anthropic.com>")
	if len(got) == 0 || !strings.Contains(got[0], "Co-Authored-By: Claude") {
		t.Fatalf("the report does not quote the offending line: %v", got)
	}
}

func TestLoadCommitPatternsRefusesAMissingFile(t *testing.T) {
	// A permissive default here would silently disarm the one rule that can
	// never be undone once a maintainer has pulled the commit.
	if _, err := LoadCommitPatterns(filepath.Join(t.TempDir(), "absent.txt")); err == nil {
		t.Fatal("a missing pattern file was accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := writeFile(empty, "# only a comment\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCommitPatterns(empty); err == nil {
		t.Fatal("a pattern file with no patterns was accepted")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
