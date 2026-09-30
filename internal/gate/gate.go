// Package gate is the human decision point. Nothing reaches implementation
// without passing through here or through internal/autogate, and the state
// table is what makes that structural rather than a matter of discipline.
//
// Every function is small and every one of them is a decision somebody will
// make on a phone, so they take a slug and return a sentence.
package gate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/store"
	"gopkg.in/yaml.v3"
)

var (
	ErrGate = errors.New("gate")
	// ErrNotProposed is returned rather than silently doing nothing, because
	// "approve" on an already-approved candidate is usually a second tap on a
	// notification and the user needs to know which it was.
	ErrNotProposed = errors.New("not awaiting approval")
)

// Auditor records a decision. Narrow on purpose: this package must not be able
// to do anything else with the audit log.
type Auditor interface {
	Record(kind, slug, detail string) error
}

// Store is the persistence this package needs.
type Store interface {
	Load(slug string) (*model.Candidate, error)
	Save(c *model.Candidate) (string, error)
	All() ([]*model.Candidate, []store.LoadResult)
}

// Approve moves a proposed candidate to approved. This is the only place a
// human approval is written.
func Approve(s Store, a Auditor, slug string) (string, error) {
	c, err := s.Load(slug)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGate, err)
	}
	if c.Status != model.StatusProposed {
		return "", fmt.Errorf("%w: %s is %q, not proposed", ErrNotProposed, slug, c.Status)
	}
	if err := model.Transition(c, model.StatusApproved, "human approval"); err != nil {
		return "", err
	}
	if _, err := s.Save(c); err != nil {
		return "", fmt.Errorf("%w: %v", ErrGate, err)
	}
	record(a, "approve", slug, c.URL)
	return fmt.Sprintf("approved %s (%s#%d)", slug, c.Repo, c.Issue), nil
}

// Reject records a human rejection with its reason.
//
// The reason is required. A rejection with no reason is indistinguishable from
// one the pipeline made itself, and the reconsider logic treats the two
// differently: a human rejection is never revisited.
func Reject(s Store, a Auditor, slug, reason string) (string, error) {
	if strings.TrimSpace(reason) == "" {
		return "", fmt.Errorf("%w: a rejection needs a reason", ErrGate)
	}
	c, err := s.Load(slug)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGate, err)
	}
	if c.Status == model.StatusAbandoned {
		return fmt.Sprintf("%s is already %s", slug, c.Status), nil
	}
	// Already rejected, but by the scorer. A machine rejection expires and the
	// candidate comes back round; a human one never does. Recording the human
	// reason over the top is not a second rejection -- the status does not
	// move, so there is no transition -- it is the difference between "parked
	// for now" and "decided", and without it a decision made once has to be
	// made again every fortnight.
	if c.Status == model.StatusRejected {
		if strings.HasPrefix(c.RejectReason, humanPrefix) {
			return fmt.Sprintf("%s was already rejected by you", slug), nil
		}
		was := c.RejectReason
		c.RejectReason = humanPrefix + reason
		if _, err := s.Save(c); err != nil {
			return "", fmt.Errorf("%w: %v", ErrGate, err)
		}
		record(a, "reject", slug, reason)
		return fmt.Sprintf("%s stays rejected, now on your reason rather than %q",
			slug, truncate(was, 60)), nil
	}
	c.RejectReason = humanPrefix + reason
	if err := model.Transition(c, model.StatusRejected, humanPrefix+reason); err != nil {
		return "", err
	}
	if _, err := s.Save(c); err != nil {
		return "", fmt.Errorf("%w: %v", ErrGate, err)
	}
	record(a, "reject", slug, reason)
	return fmt.Sprintf("rejected %s: %s", slug, reason), nil
}

// Retry puts an abandoned candidate back in the approved queue.
//
// Abandoned is terminal in the transition table, so this goes through
// model.Reopen: the history entry is marked forced and carries who did it,
// which is what lets `doctor` check every other edge strictly. v1 had three
// of these written into the JSON by hand with invented timestamps, and there
// was no way to tell them from edges the state machine had actually allowed.
//
// It exists because every failure after the human gate -- a fork that never
// appeared, a rejected push, a commit the guard refused -- lands on Abandoned,
// and without this the candidate is dead and the approval has to be made
// again from scratch.
func Retry(s Store, a Auditor, slug, actor, reason string) (string, error) {
	if strings.TrimSpace(reason) == "" {
		return "", fmt.Errorf("%w: a retry needs a reason", ErrGate)
	}
	c, err := s.Load(slug)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGate, err)
	}
	if c.Status != model.StatusAbandoned {
		return "", fmt.Errorf("%w: %s is %q; only abandoned work can be retried",
			ErrGate, slug, c.Status)
	}
	if err := model.Reopen(c, model.StatusApproved, actor, reason); err != nil {
		return "", err
	}
	// The blockers that stopped the last attempt are what the retry is for.
	// Leaving them set would have the next run refuse before it starts.
	c.Blockers = nil
	if _, err := s.Save(c); err != nil {
		return "", fmt.Errorf("%w: %v", ErrGate, err)
	}
	record(a, "retry", slug, reason)
	return fmt.Sprintf("%s is approved again: %s", slug, reason), nil
}

// Exclusions is the hand-edited list of repositories the pipeline must leave
// alone, and the ones whose CLA has been signed.
type Exclusions struct {
	Path string
}

// Exclude takes a repository off the table -- typically because the user is
// about to work on it themselves from another account, where two PRs on one
// issue would read as sockpuppeting.
//
// It also drops anything already queued for that repository. Adding the name
// to a list while four candidates sit approved would let them through on the
// next cycle, which is the opposite of what the command was for.
func Exclude(s Store, a Auditor, ex Exclusions, repo string) (string, error) {
	added, err := ex.add("exclude_repos", repo)
	if err != nil {
		return "", err
	}
	if !added {
		return fmt.Sprintf("%s is already excluded", repo), nil
	}
	record(a, "exclude_repo", "", repo)

	all, _ := s.All()
	var dropped int
	for _, c := range all {
		if c.Repo != repo {
			continue
		}
		switch c.Status {
		case model.StatusDiscovered, model.StatusScored, model.StatusProposed,
			model.StatusApproved, model.StatusAutoApproved:
		default:
			continue
		}
		c.RejectReason = repo + " manually excluded"
		if err := model.Transition(c, model.StatusRejected, "repo excluded"); err != nil {
			// An illegal edge here means the candidate is already terminal,
			// which is fine; anything else is a bug worth surfacing.
			if !errors.Is(err, model.ErrIllegalTransition) {
				return "", err
			}
			continue
		}
		if _, err := s.Save(c); err != nil {
			return "", fmt.Errorf("%w: %v", ErrGate, err)
		}
		dropped++
	}
	msg := "excluded " + repo
	if dropped > 0 {
		msg += fmt.Sprintf(", dropped %d queued candidate(s)", dropped)
	}
	return msg, nil
}

// CLASigned records that the user has signed a repository's CLA, clearing the
// blocker from every candidate in it.
func CLASigned(s Store, a Auditor, ex Exclusions, repo string) (string, error) {
	added, err := ex.add("cla_signed", repo)
	if err != nil {
		return "", err
	}
	if !added {
		return fmt.Sprintf("%s is already recorded as signed", repo), nil
	}
	record(a, "cla_signed", "", repo)

	all, _ := s.All()
	var cleared int
	for _, c := range all {
		if c.Repo != repo || len(c.Blockers) == 0 {
			continue
		}
		kept := c.Blockers[:0:0]
		for _, b := range c.Blockers {
			if !strings.Contains(strings.ToUpper(b), "CLA") {
				kept = append(kept, b)
			}
		}
		if len(kept) == len(c.Blockers) {
			continue
		}
		c.Blockers = kept
		if _, err := s.Save(c); err != nil {
			return "", fmt.Errorf("%w: %v", ErrGate, err)
		}
		cleared++
	}
	msg := "recorded CLA for " + repo
	if cleared > 0 {
		msg += fmt.Sprintf(", unblocked %d candidate(s)", cleared)
	}
	return msg, nil
}

// add appends a value to a list in the exclusions file, preserving every other
// key.
//
// Read-modify-write of the whole document rather than an append: the file also
// holds banned_ai_policy and cla_signed, and a writer that only knew about its
// own key would silently drop the others.
func (e Exclusions) add(key, value string) (bool, error) {
	doc := map[string]any{}
	b, err := os.ReadFile(e.Path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return false, fmt.Errorf("%w: parse %s: %v", ErrGate, e.Path, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	case os.IsNotExist(err):
	default:
		return false, fmt.Errorf("%w: read %s: %v", ErrGate, e.Path, err)
	}

	current := stringList(doc[key])
	for _, s := range current {
		if s == value {
			return false, nil
		}
	}
	current = append(current, value)
	sort.Strings(current)
	doc[key] = current

	out, err := yaml.Marshal(doc)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrGate, err)
	}
	if err := os.MkdirAll(filepath.Dir(e.Path), 0o755); err != nil {
		return false, fmt.Errorf("%w: %v", ErrGate, err)
	}
	if err := os.WriteFile(e.Path, out, 0o644); err != nil {
		return false, fmt.Errorf("%w: write %s: %v", ErrGate, e.Path, err)
	}
	return true, nil
}

func stringList(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []string:
		return append([]string{}, t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// record never fails the operation. The decision has already been persisted,
// and losing the audit line is worth reporting but not worth undoing a
// transition for -- there is no way to undo it that is not also a write.
func record(a Auditor, kind, slug, detail string) {
	if a != nil {
		_ = a.Record(kind, slug, detail)
	}
}

// humanPrefix marks a rejection a person made. store.ShouldReconsider keys off
// it: a machine rejection expires, this one does not.
const humanPrefix = "human rejection: "

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
