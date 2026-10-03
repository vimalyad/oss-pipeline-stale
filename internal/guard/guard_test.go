package guard

import (
	"strings"
	"testing"
)

// TestCommittedAgentFileIsCaught is the regression test for the incident this
// package exists for. A 92-line agent workflow note reached a public PR
// because the check ran against uncommitted changes only: the file had been
// committed already, so the scope was empty and everything looked clean.
func TestCommittedAgentFileIsCaught(t *testing.T) {
	committed := "A\t.agents/skills/kornia-developer/SKILL.md\nM\tkornia/core/utils.go"
	files := NetFiles(committed, "")
	if len(files) != 2 {
		t.Fatalf("net files = %v", files)
	}
	probs := Inspect(Shipment{Files: files, Diff: "some diff",
		ExistingTopLevel: map[string]bool{"kornia": true}})
	var got string
	for _, p := range probs {
		got += p.Why + "\n"
	}
	if !strings.Contains(got, "agent tooling") {
		t.Fatalf("a committed agent file must be caught:\n%s", got)
	}
}

// The other half of the same fix: a file being deleted must stop blocking, or
// the guard refuses the very commit that removes the offending file.
func TestDeletedFileNoLongerBlocks(t *testing.T) {
	committed := "A\t.agents/skills/x/SKILL.md\nM\tsrc/thing.go"
	pending := "D\t.agents/skills/x/SKILL.md"
	files := NetFiles(committed, pending)
	for _, f := range files {
		if strings.Contains(f, ".agents") {
			t.Fatalf("a removed file must leave the shipped set: %v", files)
		}
	}
	if len(Inspect(Shipment{Files: files, Diff: "d",
		ExistingTopLevel: map[string]bool{"src": true}})) != 0 {
		t.Error("removing the offending file should make the branch shippable")
	}
}

func TestNetFilesHandlesRenamesAndOrder(t *testing.T) {
	// Renames carry both names; the destination is what ships.
	files := NetFiles("R100\told/path.go\tnew/path.go", "")
	if len(files) != 1 || files[0] != "new/path.go" {
		t.Fatalf("rename resolved to %v", files)
	}
	// Order of git's output must not change the answer.
	a := NetFiles("A\tone.go\nA\ttwo.go", "D\tone.go")
	b := NetFiles("A\ttwo.go\nA\tone.go", "D\tone.go")
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("order-dependent: %v vs %v", a, b)
	}
}

func TestInspectCatchesEachCategory(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"workflow", ".github/workflows/ci.yml", "workflow changes are excluded"},
		{"agent dir", ".claude/settings.json", "agent tooling"},
		{"agent file", "AGENTS.md", "agent tooling"},
		{"cursor rules", ".cursorrules.md", "agent tooling"},
		{"pycache", "src/__pycache__/x.pyc", "build artefact"},
		{"node_modules", "web/node_modules/pkg/index.js", "build artefact"},
		{"our tooling", "uv.lock", "our own test tooling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probs := Inspect(Shipment{Files: []string{tc.path}, Diff: "d",
				ExistingTopLevel: map[string]bool{
					".github": true, ".claude": true, "src": true, "web": true}})
			var joined string
			for _, p := range probs {
				joined += p.Why + "\n"
			}
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("%s not caught; got:\n%s", tc.path, joined)
			}
		})
	}
}

// Ordinary files must pass, or the guard is useless noise.
func TestOrdinaryChangesPass(t *testing.T) {
	files := []string{"src/thing.go", "src/thing_test.go", "docs/changelog.d/123.fixed.md"}
	probs := Inspect(Shipment{Files: files, Diff: "real diff",
		ExistingTopLevel: map[string]bool{"src": true, "docs": true}})
	if len(probs) != 0 {
		t.Fatalf("false positives: %v", probs)
	}
}

func TestNewTopLevelDirIsRefused(t *testing.T) {
	probs := Inspect(Shipment{
		Files:            []string{"scratch/notes.md", "src/ok.go"},
		Diff:             "d",
		ExistingTopLevel: map[string]bool{"src": true},
	})
	var joined string
	for _, p := range probs {
		joined += p.Why
	}
	if !strings.Contains(joined, "new top-level directory") {
		t.Fatalf("got %s", joined)
	}
}

func TestScanSecrets(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"github token", "token = ghp_" + strings.Repeat("a", 30), "GitHub token"},
		{"fine grained", "github_pat_" + strings.Repeat("b", 30), "GitHub fine-grained token"},
		{"aws", "AKIA" + strings.Repeat("A", 16), "AWS access key"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----", "private key"},
		{"anthropic", "sk-ant-" + strings.Repeat("c", 25), "Anthropic API key"},
		{"slack", "xoxb-1234567890-abc", "Slack token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanSecrets(tc.text)
			if len(got) == 0 {
				t.Fatalf("missed %s", tc.name)
			}
			var found bool
			for _, g := range got {
				if g == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("got %v want %s", got, tc.want)
			}
		})
	}
	if got := ScanSecrets("ordinary code with no secrets"); len(got) != 0 {
		t.Errorf("false positive: %v", got)
	}

	// Private strings are supplied by the caller, never written here: the
	// whole point is that this file can be published.
	got := ScanSecrets("mail someone@private.example for access", "someone@private.example")
	if len(got) != 1 || got[0] != "a private address or login" {
		t.Errorf("private address not caught: %v", got)
	}
	if got := ScanSecrets("mail someone@private.example", "other@example.com"); len(got) != 0 {
		t.Errorf("false positive on an unrelated address: %v", got)
	}
}

// The PR body is read by maintainers. None of this vocabulary exists to them.
func TestCheckBodyCatchesTranscriptLanguage(t *testing.T) {
	body := "I could not run the tests because the sandbox denied the command. " +
		"Claude statically reviewed the diff instead."
	got := CheckBody(body)
	for _, want := range []string{"claude", "sandbox", "i could not", "statically reviewed"} {
		var found bool
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missed %q in %v", want, got)
		}
	}
	clean := "Guards the empty-config case and adds a regression test."
	if got := CheckBody(clean); len(got) != 0 {
		t.Errorf("false positive on a normal body: %v", got)
	}
}

// TestBodyForbiddenCatchesMessagesToTheOperator is the second incident, found
// by a dry run rather than by reading.
//
// The list held "I could not" and "I was unable", and this body was still
// passed as clean -- a model answering the pipeline rather than describing a
// change, complete with a request to paste the diff. Enumerating phrases does
// not work; the shape does.
func TestBodyForbiddenCatchesMessagesToTheOperator(t *testing.T) {
	leaked := []string{
		`I don't see any diff content in your message — the request says "the diff on stdin" but none was included.`,
		"Could you paste the diff itself, or point me to a commit range?",
		"I can't write an accurate description without seeing the actual code changes.",
		"I cannot verify this claim.",
		"Let me know if you want me to extend it.",
		"I was unable to run the tests.",
		"I could not reproduce the failure.",
		"Please provide the diff so I can describe it.",
	}
	for _, body := range leaked {
		if got := CheckBody(body); len(got) == 0 {
			t.Errorf("passed as clean: %q", body)
		}
	}
}

// TestBodyForbiddenStillCatchesTooling.
func TestBodyForbiddenStillCatchesTooling(t *testing.T) {
	for _, body := range []string{
		"Generated with Claude.", "Prepared with AI assistance.",
		"The sandbox denied the command.", "An assistant reviewed this.",
		"This required approval in the harness.",
	} {
		if got := CheckBody(body); len(got) == 0 {
			t.Errorf("passed as clean: %q", body)
		}
	}
}

// TestAGenuineDescriptionIsNotRejected. The bias is towards catching, but a
// normal pull request body must still get through or every submission falls
// back to the minimal form.
func TestAGenuineDescriptionIsNotRejected(t *testing.T) {
	for _, body := range []string{
		"`.helmignore` patterns were not applied to symlinked paths, so a chart " +
			"packaged from a directory containing a symlink shipped files the " +
			"ignore file excluded.\n\n**Change.** `symwalk` now resolves each link " +
			"and applies the same filter to the target, matching the behaviour " +
			"`Walk` documents.\n\n**Verification.** `go test ./internal/sympath " +
			"./pkg/chart/v2/loader` passes; the new case fails before the change.",
		"Adds a regression test for the reported crash and fixes the nil dereference in `Load`.",
		"The parser accepted trailing commas. It no longer does, and the existing " +
			"fixtures cover both forms.",
	} {
		if got := CheckBody(body); len(got) != 0 {
			t.Errorf("a genuine description was rejected for %v:\n%s", got, body)
		}
	}
}

// TestUnknownAgentScaffoldingIsCaughtByTheGeneralRule is the test that
// matters more than the name list.
//
// .serena/ appeared in a clone on 3 October and was not in AgentArtefacts.
// New tools will keep appearing, so the protection cannot be a list of their
// names; it has to be the question "does a bug fix create a top-level
// directory this repository did not have?". The answer is no, and that is
// what refused it.
func TestUnknownAgentScaffoldingIsCaughtByTheGeneralRule(t *testing.T) {
	for _, path := range []string{
		".serena/project.yml",
		".some-tool-nobody-has-heard-of/state.json",
		".a-tool-invented-next-year/memories/notes.md",
	} {
		s := Shipment{
			Files:            []string{"pkg/fix.go", path},
			Diff:             "diff --git a/pkg/fix.go b/pkg/fix.go\n+real change\n",
			ExistingTopLevel: map[string]bool{"pkg": true, "cmd": true, ".github": true},
		}
		problems := Inspect(s)
		if len(problems) == 0 {
			t.Fatalf("%s shipped clean", path)
		}
		dir, _, _ := strings.Cut(path, "/")
		var saw bool
		for _, p := range problems {
			if strings.Contains(p.Why, dir) {
				saw = true
			}
		}
		if !saw {
			t.Fatalf("%s: nothing named the offending directory: %v", path, problems)
		}
	}
}

// And a dot-directory the repository already has must not be flagged: plenty
// of projects ship .github, .vscode or .devcontainer of their own.
func TestExistingDotDirectoriesAreNotScaffolding(t *testing.T) {
	s := Shipment{
		Files:            []string{".github/dependabot.yml", "pkg/fix.go"},
		Diff:             "diff --git a/pkg/fix.go b/pkg/fix.go\n+real change\n",
		ExistingTopLevel: map[string]bool{"pkg": true, ".github": true},
	}
	for _, p := range Inspect(s) {
		if strings.Contains(p.Why, "top-level") {
			t.Fatalf("flagged a directory the repository already had: %s", p.Why)
		}
	}
}
