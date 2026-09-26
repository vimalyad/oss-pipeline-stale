package propose

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vimalyad/osspipeline/internal/policy"
	"github.com/vimalyad/osspipeline/internal/profile"
	"github.com/vimalyad/osspipeline/internal/store"
)

// TestLiveRender builds the proposal report from the real state, for
// comparison against the Python implementation.
//
//	OSSP_LIVE=1 go test ./internal/propose -run TestLiveRender -v
func TestLiveRender(t *testing.T) {
	if os.Getenv("OSSP_LIVE") == "" {
		t.Skip("set OSSP_LIVE=1")
	}
	root := "/Users/vimalkumaryadav/oss-pipeline"
	cfg, err := policy.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	prof, err := profile.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	all, bad := store.New(root).All()
	if len(bad) > 0 {
		t.Fatalf("%d unreadable", len(bad))
	}

	proposed, rejected, ops := Classify(all, prof)
	fmt.Printf("proposed=%d rejected=%d comment-opportunities=%d\n",
		len(proposed), len(rejected), len(ops))
	fmt.Println("ranked order:")
	for _, e := range proposed {
		d := "unmatched"
		if e.HasDomain {
			d = fmt.Sprintf("%s w%d", e.Domain.ID, weight(e))
		}
		fmt.Printf("  %-34s %-16s blockers=%d penalties=%d reactions=%d\n",
			e.Candidate.Slug(), d, len(e.Candidate.Blockers),
			len(e.Candidate.SoftPenalties), e.Candidate.Reactions)
	}

	out := Render(all, prof, cfg.Policy.Caps, time.Now())
	if err := os.WriteFile("/tmp/go_propose.md", []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("rendered %d chars, %d headings\n", len(out),
		strings.Count(out, "\n#"))
}
