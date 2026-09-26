// Package store persists candidates as one JSON file each.
//
// Not a database, deliberately: when an unattended run wedges, being able to
// read the state with `cat` and correct it in a text editor has repeatedly
// been what made the failure diagnosable.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vimalyad/osspipeline/internal/model"
	"time"
)

// Store reads and writes candidate files under a root directory.
type Store struct {
	root string // the pipeline root, containing state/
}

func New(root string) *Store { return &Store{root: root} }

func (s *Store) dir() string      { return filepath.Join(s.root, "state", "candidates") }
func (s *Store) reposDir() string { return filepath.Join(s.root, "state", "repos") }

// PathFor is the file a slug lives at.
func (s *Store) PathFor(slug string) string {
	return filepath.Join(s.dir(), slug+".json")
}

// Load reads one candidate.
func (s *Store) Load(slug string) (*model.Candidate, error) {
	b, err := os.ReadFile(s.PathFor(slug))
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", slug, err)
	}
	return decode(b, slug)
}

func decode(b []byte, slug string) (*model.Candidate, error) {
	var c model.Candidate
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", slug, err)
	}
	// A status outside the table means the file was hand-edited wrongly.
	// Say so rather than carrying on with something the state machine cannot
	// reason about -- v1 tolerated this and accumulated three bad files.
	if !c.Status.Valid() {
		return nil, fmt.Errorf("parse %s: unknown status %q", slug, c.Status)
	}
	return &c, nil
}

// Save writes a candidate atomically: a crash mid-write must not truncate a
// file the next run has to read.
func (s *Store) Save(c *model.Candidate) (string, error) {
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	final := s.PathFor(c.Slug())
	tmp, err := os.CreateTemp(s.dir(), ".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, nil
}

// LoadResult carries one file's outcome so a single corrupt file does not
// hide the other 223.
type LoadResult struct {
	Slug string
	Cand *model.Candidate
	Err  error
}

// All reads every candidate. Unreadable files are reported, not skipped
// silently, and never abort the sweep.
func (s *Store) All() ([]*model.Candidate, []LoadResult) {
	paths, _ := filepath.Glob(filepath.Join(s.dir(), "*.json"))
	sort.Strings(paths)
	var ok []*model.Candidate
	var bad []LoadResult
	for _, p := range paths {
		slug := strings.TrimSuffix(filepath.Base(p), ".json")
		b, err := os.ReadFile(p)
		if err != nil {
			bad = append(bad, LoadResult{Slug: slug, Err: err})
			continue
		}
		c, err := decode(b, slug)
		if err != nil {
			bad = append(bad, LoadResult{Slug: slug, Err: err})
			continue
		}
		ok = append(ok, c)
	}
	return ok, bad
}

// ByStatus returns candidates in any of the given statuses.
func (s *Store) ByStatus(want ...model.Status) []*model.Candidate {
	set := make(map[model.Status]bool, len(want))
	for _, w := range want {
		set[w] = true
	}
	all, _ := s.All()
	var out []*model.Candidate
	for _, c := range all {
		if set[c.Status] {
			out = append(out, c)
		}
	}
	return out
}

// LoadRepoFacts reads the cached per-repo gates, if present.
func (s *Store) LoadRepoFacts(repo string) (*model.RepoFacts, error) {
	name := strings.ReplaceAll(repo, "/", "__") + ".json"
	b, err := os.ReadFile(filepath.Join(s.reposDir(), name))
	if err != nil {
		return nil, err
	}
	var f model.RepoFacts
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse facts %s: %w", repo, err)
	}
	return &f, nil
}

// SaveRepoFacts writes the weekly repository cache.
//
// Written whole and atomically, like candidates: a torn facts file is loaded
// on the next run as an unparseable cache, which silently refetches every
// repository and burns the API budget a weekly cache exists to protect.
func (s *Store) SaveRepoFacts(f *model.RepoFacts) error {
	if f == nil || f.Repo == "" {
		return fmt.Errorf("store: facts with no repo")
	}
	if err := os.MkdirAll(s.reposDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	name := strings.ReplaceAll(f.Repo, "/", "__") + ".json"
	final := filepath.Join(s.reposDir(), name)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Rejections fall into two kinds.
//
// Structural ones are facts about the repository or a human's decision, and do
// not change on their own. Transient ones describe a moment in time -- someone
// was mid-pull-request, someone had just claimed it, the thread had not
// converged yet -- and all of those expire.
//
// Treating every rejection as permanent quietly discards the best candidates:
// the two strongest issues found on the first real sweep were both rejected as
// "claimed", and claims lapse.
var (
	transientRejections = []string{
		"contest=active_pr", "contest=claimed", "claimed by", "deferred:",
		"thread has not converged", "no maintainer acceptance",
		"no stated approach", "harvest/brief failed",
	}
	structuralRejections = []string{
		"bans ai", "no contributing", "requires a cla", "manually excluded",
		"already touched by another of your accounts", "docs/typo-only",
		"no test suite", "unreceptive",
	}
)

// IsTransient reports whether a rejection reason expires.
//
// Structural wins over transient when both match: a reason can mention a
// deferral and a missing CONTRIBUTING file, and the second one is still true
// next week.
func IsTransient(reason string) bool {
	low := strings.ToLower(reason)
	for _, m := range structuralRejections {
		if strings.Contains(low, m) {
			return false
		}
	}
	for _, m := range transientRejections {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// RejectedAt is when the candidate was last rejected, from its own history.
func RejectedAt(c *model.Candidate) (time.Time, bool) {
	for i := len(c.History) - 1; i >= 0; i-- {
		if model.Status(c.History[i].To) != model.StatusRejected {
			continue
		}
		if t, err := time.Parse(time.RFC3339, c.History[i].At); err == nil {
			return t, true
		}
		// A hand-edited or synthetic marker: keep looking rather than
		// treating an unparseable date as "never".
	}
	return time.Time{}, false
}

// ShouldReconsider reports whether a candidate deserves another look.
//
// Anything never seen is always considered. Anything live or terminal-by-
// success is not. A rejection is revisited only when it was transient and the
// cooldown has passed -- and never when a human made it, because asking again
// about something a person already declined is how a pipeline becomes noise.
func (s *Store) ShouldReconsider(slug string, now time.Time, afterDays int) bool {
	p := s.PathFor(slug)
	info, err := os.Stat(p)
	if err != nil {
		return true // never seen
	}
	c, err := s.Load(slug)
	if err != nil {
		return true // unreadable: look again rather than skip silently
	}
	if c.Status != model.StatusRejected {
		return false
	}
	if strings.HasPrefix(strings.ToLower(c.RejectReason), "human rejection") {
		return false
	}
	if !IsTransient(c.RejectReason) {
		return false
	}
	when, ok := RejectedAt(c)
	if !ok {
		when = info.ModTime()
	}
	return now.Sub(when) >= time.Duration(afterDays)*24*time.Hour
}
