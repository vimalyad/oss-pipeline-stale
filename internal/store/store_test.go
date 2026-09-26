package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vimalyad/osspipeline/internal/model"
	"time"
)

// root finds the pipeline root from the test's working directory.
func root(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	return filepath.Dir(filepath.Dir(wd)) // internal/store -> repo root
}

// TestLoadsEveryV1File is the parity gate for group 1.
//
// The Go build must read v1's existing state with no migration, so that both
// binaries can run against the same files and their output be compared. If
// this fails, the rewrite has diverged from the data it has to inherit.
func TestLoadsEveryV1File(t *testing.T) {
	s := New(root(t))
	if _, err := os.Stat(s.dir()); os.IsNotExist(err) {
		t.Skip("no v1 state on this machine")
	}
	ok, bad := s.All()
	for _, b := range bad {
		t.Errorf("could not load %s: %v", b.Slug, b.Err)
	}
	if len(ok) == 0 {
		t.Fatal("loaded no candidates at all")
	}
	t.Logf("loaded %d candidates, %d unreadable", len(ok), len(bad))

	// Spot-check that nested structures actually decoded, not just the
	// top-level scalars: a wrong tag on Brief would leave it nil and the
	// count above would still look healthy.
	var withBrief, withFacts, withSignal int
	for _, c := range ok {
		if c.Slug() == "" || c.Repo == "" || c.Issue == 0 {
			t.Errorf("degenerate candidate: %+v", c)
		}
		if c.Brief != nil {
			withBrief++
		}
		if c.Facts != nil {
			withFacts++
		}
		if c.PRSignal != nil {
			withSignal++
		}
	}
	if withBrief == 0 || withFacts == 0 {
		t.Errorf("no candidate decoded a Brief (%d) or Facts (%d) -- json tags are wrong",
			withBrief, withFacts)
	}
	t.Logf("with brief: %d, with facts: %d, with pr_signal: %d",
		withBrief, withFacts, withSignal)
}

// TestRoundTripPreservesEveryField guards the same property from the other
// side: writing a candidate and reading it back must not lose anything.
func TestRoundTripPreservesEveryField(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	n := 42
	want := &model.Candidate{
		Repo: "acme/widget", Issue: 7, Title: "t", URL: "u",
		Status: model.StatusChangesRequested,
		Labels: []string{"bug"}, Comments: 3, Reactions: 1,
		Contest:  model.ContestStalePR,
		PRSignal: &model.PRSignal{Number: 9, Author: "someone", Reasons: []string{"stale"}},
		Brief:    &model.Brief{MaintainerDesiredApproach: "do x", OpenQuestions: []string{}},
		Facts:    &model.RepoFacts{Repo: "acme/widget", Stars: 5, RequiresDCO: true},
		PRNumber: &n,
		History:  []model.HistoryEntry{{At: "now", From: "pr_open", To: "changes_requested"}},
	}
	if _, err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(want.Slug())
	if err != nil {
		t.Fatal(err)
	}
	if got.PRSignal == nil || got.PRSignal.Number != 9 {
		t.Error("pr_signal lost")
	}
	if got.Brief == nil || got.Brief.MaintainerDesiredApproach != "do x" {
		t.Error("brief lost")
	}
	if got.Facts == nil || !got.Facts.RequiresDCO {
		t.Error("facts lost")
	}
	if got.PRNumber == nil || *got.PRNumber != 42 {
		t.Error("pr_number lost")
	}
	if got.Status != model.StatusChangesRequested || got.Contest != model.ContestStalePR {
		t.Error("enums lost")
	}
}

func TestRejectsUnknownStatus(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "state", "candidates"), 0o755)
	os.WriteFile(filepath.Join(dir, "state", "candidates", "a__b__1.json"),
		[]byte(`{"repo":"a/b","issue":1,"status":"nonsense"}`), 0o644)
	if _, err := New(dir).Load("a__b__1"); err == nil {
		t.Fatal("a hand-edited bad status must be reported, not accepted")
	}
}

// TestIsTransient: treating every rejection as permanent quietly discards the
// best candidates -- the two strongest issues on the first real sweep were both
// rejected as "claimed", and claims lapse.
func TestIsTransient(t *testing.T) {
	tests := []struct {
		reason string
		want   bool
	}{
		{"contest=active_pr (author active 22d ago)", true},
		{"claimed by @someone -- respect the claim", true},
		{"deferred: exceeded max_harvest=12 this run", true},
		{"no maintainer acceptance (no triage-gated label)", true},
		{"thread has not converged", true},

		{"repo bans AI-assisted contributions", false},
		{"no CONTRIBUTING file", false},
		{"requires a CLA", false},
		{"kornia/kornia manually excluded", false},
		{"docs/typo-only change", false},
		{"", false},
		{"something nobody wrote a rule for", false},

		// Structural wins when both match: a reason can mention a deferral and
		// a missing CONTRIBUTING file, and the second is still true next week.
		{"deferred: exceeded max_harvest; also no CONTRIBUTING file", false},
	}
	for _, tt := range tests {
		if got := IsTransient(tt.reason); got != tt.want {
			t.Errorf("IsTransient(%q) = %v, want %v", tt.reason, got, tt.want)
		}
	}
}

func TestShouldReconsider(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	ago := func(d int) string { return now.AddDate(0, 0, -d).Format(time.RFC3339) }

	tests := []struct {
		name   string
		cand   *model.Candidate
		want   bool
		reason string
	}{{
		name: "a transient rejection past the cooldown",
		cand: &model.Candidate{Repo: "a/b", Issue: 1, Status: model.StatusRejected,
			RejectReason: "contest=active_pr",
			History:      []model.HistoryEntry{{At: ago(20), To: "rejected"}}},
		want: true,
	}, {
		name: "a transient rejection inside the cooldown",
		cand: &model.Candidate{Repo: "a/b", Issue: 2, Status: model.StatusRejected,
			RejectReason: "contest=active_pr",
			History:      []model.HistoryEntry{{At: ago(3), To: "rejected"}}},
		want: false,
	}, {
		name: "a structural rejection, however old",
		cand: &model.Candidate{Repo: "a/b", Issue: 3, Status: model.StatusRejected,
			RejectReason: "requires a CLA",
			History:      []model.HistoryEntry{{At: ago(400), To: "rejected"}}},
		want: false,
	}, {
		// Asking again about something a person already declined is how a
		// pipeline becomes noise.
		name: "a human rejection is never revisited",
		cand: &model.Candidate{Repo: "a/b", Issue: 4, Status: model.StatusRejected,
			RejectReason: "human rejection: not worth the review time",
			History:      []model.HistoryEntry{{At: ago(400), To: "rejected"}}},
		want: false,
	}, {
		name: "a live candidate is not reconsidered",
		cand: &model.Candidate{Repo: "a/b", Issue: 5, Status: model.StatusPROpen},
		want: false,
	}, {
		name: "a merged candidate is not reconsidered",
		cand: &model.Candidate{Repo: "a/b", Issue: 6, Status: model.StatusMerged},
		want: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(t.TempDir())
			if _, err := s.Save(tt.cand); err != nil {
				t.Fatal(err)
			}
			if got := s.ShouldReconsider(tt.cand.Slug(), now, 14); got != tt.want {
				t.Errorf("= %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNeverSeenIsAlwaysConsidered, and an unreadable file too: skipping a
// candidate because its file is corrupt would hide the corruption.
func TestNeverSeenOrUnreadableIsConsidered(t *testing.T) {
	now := time.Now()
	s := New(t.TempDir())
	if !s.ShouldReconsider("never__seen__1", now, 14) {
		t.Error("a candidate never seen was skipped")
	}
	if err := os.MkdirAll(filepath.Dir(s.PathFor("bad__file__1")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.PathFor("bad__file__1"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !s.ShouldReconsider("bad__file__1", now, 14) {
		t.Error("an unreadable candidate was silently skipped")
	}
}

// TestAnUnparseableHistoryDateFallsBackToTheFile: a hand-edited marker must not
// make a rejection look like it happened at the zero time and so always be due.
func TestAnUnparseableHistoryDateFallsBackToTheFile(t *testing.T) {
	s := New(t.TempDir())
	c := &model.Candidate{Repo: "a/b", Issue: 9, Status: model.StatusRejected,
		RejectReason: "contest=active_pr",
		History:      []model.HistoryEntry{{At: "some time last week", To: "rejected"}}}
	if _, err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	if _, ok := RejectedAt(c); ok {
		t.Fatal("an unparseable date was accepted as a timestamp")
	}
	// The file was just written, so its mtime is now: inside any cooldown.
	if s.ShouldReconsider(c.Slug(), time.Now(), 14) {
		t.Error("fell back to the zero time instead of the file's mtime")
	}
}
