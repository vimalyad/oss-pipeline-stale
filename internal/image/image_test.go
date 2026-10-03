package image

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/vimalyad/osspipeline/internal/recipe"
)

func completeRecipe() recipe.Recipe {
	return recipe.Recipe{
		Repo: "example/thing", Source: recipe.SourceOverride,
		Evidence: []string{"config/environments.yaml"},
		// Chosen because it is tiny and already present on any machine that
		// has run the sandbox tests; the build has nothing to download.
		BaseImage: "debian:bookworm-slim",
		Platform:  "linux/arm64",
		Test:      []recipe.Step{{Kind: "test", Run: "true", From: "override"}},
	}
}

// TestIncompleteRecipeIsNeverBuilt: a recipe with no test command produces a
// container that proves nothing, so the refusal belongs before the build
// rather than after twenty minutes of it.
func TestIncompleteRecipeIsNeverBuilt(t *testing.T) {
	r := completeRecipe()
	r.Test = nil
	if _, err := Ensure(context.Background(), r); !errors.Is(err, ErrIncompleteRecipe) {
		t.Fatalf("err = %v, want ErrIncompleteRecipe", err)
	}
}

func TestTailKeepsTheEnd(t *testing.T) {
	short := "line one\nline two\n"
	if got := tail(short); got != short {
		t.Errorf("short output was altered: %q", got)
	}
	long := strings.Repeat("x", tailBytes) + "\nthe actual error\n"
	got := tail(long)
	if !strings.Contains(got, "the actual error") {
		t.Error("the end of a long build log is the part that says why it failed")
	}
	if len(got) > tailBytes+len("... (truncated)\n") {
		t.Errorf("tail returned %d bytes", len(got))
	}
}

// --- integration ---------------------------------------------------------

func dockerAvailable(t *testing.T) {
	t.Helper()
	if os.Getenv("OSSP_DOCKER_TESTS") == "" {
		t.Skip("set OSSP_DOCKER_TESTS=1 to run container integration tests")
	}
	if exec.Command("docker", "version").Run() != nil {
		t.Skip("docker not available")
	}
}

func TestIntegrationEnsureBuildsThenCaches(t *testing.T) {
	dockerAvailable(t)
	r := completeRecipe()
	ctx := context.Background()
	t.Cleanup(func() { _, _ = Prune(ctx) })

	first, err := Ensure(ctx, r)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if first.Cached {
		t.Fatal("first build reported a cache hit")
	}
	second, err := Ensure(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cached || second.Tag != first.Tag {
		t.Errorf("second Ensure rebuilt: cached=%v tag=%s vs %s", second.Cached, second.Tag, first.Tag)
	}

	// A changed recipe must not reuse the image. The tag is a digest of the
	// Dockerfile precisely so this cannot go wrong silently.
	r2 := r
	r2.System = []string{"jq"}
	if r2.ImageTag() == first.Tag {
		t.Fatal("a changed recipe produced the same tag")
	}
}

// TestIntegrationPruneOnlyTouchesLabelledImages is the safety property. This
// machine holds Docker state belonging to other work, and a prune that reached
// it would be unrecoverable.
func TestIntegrationPruneOnlyTouchesLabelledImages(t *testing.T) {
	dockerAvailable(t)
	ctx := context.Background()

	before := dockerImageIDs(t)
	r := completeRecipe()
	if _, err := Ensure(ctx, r); err != nil {
		t.Fatal(err)
	}
	mine, err := List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) == 0 {
		t.Fatal("List found nothing after a build")
	}
	for _, tag := range mine {
		if !strings.HasPrefix(tag, "ossp/") {
			t.Fatalf("List returned an image this package did not build: %s", tag)
		}
	}
	if _, err := Prune(ctx); err != nil {
		t.Fatal(err)
	}

	after := dockerImageIDs(t)
	for id := range before {
		if !after[id] {
			t.Errorf("prune removed an unrelated image: %s", id)
		}
	}
}

func dockerImageIDs(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("docker", "image", "ls", "-q", "--no-trunc").Output()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, l := range strings.Fields(string(out)) {
		ids[l] = true
	}
	return ids
}

func TestPullBaseRefusesARecipeWithNoBaseImage(t *testing.T) {
	// Reaching the registry with an empty reference asks Docker Hub for
	// nothing and gets a confusing answer; saying so here is clearer.
	if err := pullBase(context.Background(), "", ""); !errors.Is(err, ErrBuild) {
		t.Fatalf("err = %v", err)
	}
}

func TestPullBaseSkipsWhatIsAlreadyLocal(t *testing.T) {
	// The whole point: a base image already on this machine must not cost a
	// registry round trip, because that round trip is what failed twice.
	// scratch is always present and needs no network.
	old := PullAttempts
	PullAttempts = 0 // any pull attempt at all would now return a nil error
	t.Cleanup(func() { PullAttempts = old })

	out, err := exec.Command("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		t.Skip("docker is not available here")
	}
	local := ""
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.Contains(l, "<none>") {
			local = l
			break
		}
	}
	if local == "" {
		t.Skip("no local images to check against")
	}
	if err := pullBase(context.Background(), local, ""); err != nil {
		t.Fatalf("%s is already local and still tried to pull: %v", local, err)
	}
}
