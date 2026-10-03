// Package guard holds the checks that stand between a generated patch and a
// public pull request under the user's name.
//
// Every rule here exists because something got through. They are pure
// functions over paths and text so they can be tested exhaustively without a
// git repository, and so the expensive part (working out what a branch ships)
// is separable from the cheap part (deciding whether it may ship).
package guard

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Junk is build output that must never appear in a diff. uv.lock is listed
// separately because it is OUR fault: the test command creates it. Tooling
// used to validate a change must not leak into the change.
var Junk = regexp.MustCompile(
	`(^|/)(__pycache__/|\.pytest_cache/|node_modules/|\.DS_Store$|[^/]+\.pyc$|\.ruff_cache/)`)

// OurTooling is produced by this pipeline's own verification, not by the fix.
var OurTooling = map[string]bool{"uv.lock": true}

// AgentArtefacts is agent scaffolding. An auto-fix run once committed a
// 92-line agent workflow note into a real public PR: it was not build junk and
// not a workflow file, so nothing that existed at the time caught it.
// The name list is the actionable half -- "never ship this" reads better than
// "new top-level directory" -- but it is not the protection. New tools appear
// faster than the list grows: .serena turned up in a clone on 3 October and
// was not here. What caught it is NewTopLevelDirs, which asks the general
// question instead, and that is the rule to keep working.
var AgentArtefacts = regexp.MustCompile(
	`(?i)(^|/)(\.agents?|\.claude|\.cursor|\.aider[^/]*|\.serena|\.windsurf|\.continue|` +
		`\.github/copilot[^/]*)/` +
		`|(^|/)(SKILL|AGENTS|CLAUDE|GEMINI|\.cursorrules)\.md$` +
		`|(^|/)\.aider\.`)

type secretPattern struct {
	re   *regexp.Regexp
	name string
}

var secrets = []secretPattern{
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`), "GitHub token"},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), "GitHub fine-grained token"},
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "AWS access key"},
	{regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----`), "private key"},
	{regexp.MustCompile(`sk-[A-Za-z0-9]{32,}`), "API secret key"},
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`), "Anthropic API key"},
	{regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`), "Slack token"},
}

// ScanSecrets names every kind of credential found in text.
//
// `private` holds literal strings that must never be published but are not a
// recognisable credential shape -- the user's other email addresses, for
// instance. They are passed in from the gitignored identity configuration
// rather than written here: hardcoding the address this scanner exists to
// suppress would publish it the moment this repository became public.
func ScanSecrets(text string, private ...string) []string {
	var found []string
	for _, s := range secrets {
		if s.re.MatchString(text) {
			found = append(found, s.name)
		}
	}
	low := strings.ToLower(text)
	for _, p := range private {
		if p != "" && strings.Contains(low, strings.ToLower(p)) {
			found = append(found, "a private address or login")
			break
		}
	}
	return found
}

// BodyForbidden is language that must never reach a public PR description.
//
// The implementer's closing message is addressed to the pipeline operator, not
// to maintainers. Publishing it verbatim once put an agent transcript on a
// real PR: first-person notes about denied commands, a claim the tests had not
// been run when they had, and an admission of guessing a PR number.
// BodyForbidden catches text that must never reach a public comment or pull
// request body.
//
// Two groups, and the second is the one that was missing. The first names the
// tooling: a maintainer reading about a sandbox or a model learns something
// true and irrelevant, and it reads as carelessness.
//
// The second catches a message addressed to the pipeline operator rather than
// to a maintainer. Enumerating phrases did not work -- the list held "I could
// not" and "I was unable", and a dry run still produced "I don't see any diff
// content in your message ... Could you paste the diff itself?" and was passed
// as clean. So this matches the shape instead: first-person statements of
// incapacity, and second-person questions about the request. A pull request
// description never asks its reader for the diff.
//
// The bias is deliberate. A false positive costs a plain fallback description;
// a false negative puts an agent transcript on a public pull request under the
// user's name, which has happened once already.
var BodyForbidden = regexp.MustCompile(
	`(?i)(\b(claude|anthropic|LLM|language model|AI|assistant|` +
		`sandbox|harness|requires approval|this session|statically reviewed)\b` +
		`|\bI (?:don'?t|do not|can'?t|cannot|could not|couldn'?t|was unable|am unable|` +
		`haven'?t|have not|guessed|need(?: you)? to see)\b` +
		`|\b(?:could|can|would) you\b` +
		`|\byour (?:message|request|prompt|diff)\b` +
		`|\b(?:paste|provide|share) the\b` +
		`|\blet me know\b` +
		`|\bon stdin\b)`)

// CheckBody returns the forbidden phrases present in a PR body or comment.
func CheckBody(text string) []string {
	m := BodyForbidden.FindAllString(text, -1)
	seen := map[string]bool{}
	var out []string
	for _, s := range m {
		l := strings.ToLower(s)
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// Shipment is what a branch will actually put in front of maintainers.
//
// Computed as the NET set: files the branch adds or modifies versus upstream,
// minus anything the working tree deletes. Checking only uncommitted work is
// how agent scaffolding reached a public PR -- the rule matched the path
// perfectly, but the file had been committed already, so the check ran against
// an empty set and reported clean.
type Shipment struct {
	Files []string
	Diff  string
	// ExistingTopLevel is what the repo had before this branch.
	ExistingTopLevel map[string]bool
	// Private are literal strings that must never be published, supplied by
	// the caller from the gitignored identity file.
	Private []string
}

// Problem is one reason a branch must not be submitted.
type Problem struct {
	Path string
	Why  string
}

func (p Problem) String() string { return p.Why }

// Inspect applies every path and content rule to a shipment.
func Inspect(s Shipment) []Problem {
	var out []Problem

	if strings.TrimSpace(s.Diff) == "" {
		out = append(out, Problem{Why: "empty diff -- nothing to submit"})
	}
	for _, name := range ScanSecrets(s.Diff, s.Private...) {
		out = append(out, Problem{Why: fmt.Sprintf("diff contains a %s -- refusing to push", name)})
	}
	for _, d := range NewTopLevelDirs(s) {
		out = append(out, Problem{Path: d, Why: fmt.Sprintf(
			"diff creates a new top-level directory %q; a fix should not", d)})
	}
	for _, path := range s.Files {
		if path == "" {
			continue
		}
		switch {
		case strings.HasPrefix(path, ".github/workflows"):
			out = append(out, Problem{path, fmt.Sprintf(
				"diff touches %s; workflow changes are excluded", path)})
		case AgentArtefacts.MatchString(path):
			out = append(out, Problem{path, fmt.Sprintf(
				"diff adds agent tooling %s; never ship this", path)})
		case Junk.MatchString(path):
			out = append(out, Problem{path, fmt.Sprintf(
				"diff contains build artefact %s", path)})
		case OurTooling[path]:
			out = append(out, Problem{path, fmt.Sprintf(
				"diff contains %s, created by our own test tooling -- clean it before committing", path)})
		}
	}
	return out
}

// NewTopLevelDirs are directories the diff invents.
//
// A bug fix does not create a new top-level directory. When one appears it is
// almost always the implementer building scaffolding for itself.
func NewTopLevelDirs(s Shipment) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range s.Files {
		dir, _, ok := strings.Cut(p, "/")
		if !ok || dir == "" || s.ExistingTopLevel[dir] || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// nameStatus is one line of `git diff --name-status`.
type nameStatus struct {
	Status string
	Path   string
}

// parseNameStatus reads git's name-status output, taking the LAST
// tab-separated field as the path so a rename (which carries both the old and
// the new name) resolves to the destination.
func parseNameStatus(s string) []nameStatus {
	var out []nameStatus
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		st := strings.TrimSpace(fields[0])
		path := fields[len(fields)-1]
		if st == "" || path == "" {
			continue
		}
		out = append(out, nameStatus{Status: st, Path: path})
	}
	return out
}

// NetFiles computes the shipped set from git's two name-status listings: what
// the branch committed versus upstream, and what the working tree still has
// pending. A file committed earlier and deleted now is not something this PR
// adds, and a file added only in the working tree is.
func NetFiles(committedNameStatus, pendingNameStatus string) []string {
	set := map[string]bool{}
	for _, e := range parseNameStatus(committedNameStatus) {
		switch e.Status[:1] {
		case "A", "M", "R", "C":
			set[e.Path] = true
		}
	}
	// Collect pending adds and deletes separately, then subtract. Doing it in
	// one pass would make the result depend on the order git happens to list
	// the files in.
	pendingAdd, pendingDel := map[string]bool{}, map[string]bool{}
	for _, e := range parseNameStatus(pendingNameStatus) {
		if e.Status[:1] == "D" {
			pendingDel[e.Path] = true
		} else {
			pendingAdd[e.Path] = true
		}
	}
	for p := range pendingAdd {
		set[p] = true
	}
	for p := range pendingDel {
		delete(set, p)
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
