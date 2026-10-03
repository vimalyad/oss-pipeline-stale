// Package image builds and caches the container images that internal/recipe
// describes.
//
// Two decisions shape it. The build context holds nothing but the generated
// Dockerfile -- the source tree is bind-mounted at run time, never copied, so
// one image serves every branch and an agent's edits land on the host clone
// directly. A context that copied the checkout would ship 895 MB to the daemon
// for kornia alone, per build, to achieve less.
//
// And everything this package creates carries ossp.managed=1. Pruning filters
// on that label and nothing else, because this machine holds a lot of Docker
// state that has nothing to do with the pipeline, and a sweep that removed it
// would be unrecoverable.
package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vimalyad/osspipeline/internal/recipe"
)

var (
	ErrBuild = errors.New("image build")
	// ErrIncompleteRecipe is returned rather than building something that
	// cannot verify anything. An image with no test command to run is a
	// container that proves nothing.
	ErrIncompleteRecipe = errors.New("recipe cannot verify a patch")
)

const (
	// ManagedLabel is the only thing Prune filters on.
	ManagedLabel = "ossp.managed=1"
	repoLabel    = "ossp.repo"
	buildTimeout = 30 * time.Minute
	pullTimeout  = 10 * time.Minute
	tailBytes    = 8000
)

// Built describes the outcome of Ensure.
type Built struct {
	Tag string
	// Cached is true when the image already existed, which is the common case:
	// the tag is a digest of the Dockerfile, so an unchanged recipe never
	// rebuilds and a changed one never reuses a stale image.
	Cached   bool
	Duration time.Duration
	Output   string
}

// Ensure returns a built image for the recipe, building it only if the exact
// Dockerfile has not been built before.
func Ensure(ctx context.Context, r recipe.Recipe) (Built, error) {
	if !r.Complete() {
		return Built{}, fmt.Errorf("%s: %w", r.Repo, ErrIncompleteRecipe)
	}
	tag := r.ImageTag()
	if exists(ctx, tag) {
		return Built{Tag: tag, Cached: true}, nil
	}

	dir, err := os.MkdirTemp("", "ossp-build-")
	if err != nil {
		return Built{}, fmt.Errorf("%w: context: %v", ErrBuild, err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(r.Dockerfile()), 0o644); err != nil {
		return Built{}, fmt.Errorf("%w: write Dockerfile: %v", ErrBuild, err)
	}

	// Pull the base image before building, with retries.
	//
	// BuildKit resolves `FROM` against the registry itself, and when Docker
	// Hub is slow it gives up with `DeadlineExceeded: context deadline
	// exceeded` on a timeout we do not set and cannot lengthen. That failed
	// the same candidate on two consecutive scheduled runs -- an approved
	// patch going unwritten for two days because a metadata fetch was slow at
	// 09:30. A separate pull is retryable, says plainly what went wrong, and
	// leaves the metadata cached locally so the build's own resolution is a
	// local lookup.
	if err := pullBase(ctx, r.BaseImage, r.Platform); err != nil {
		return Built{Tag: tag}, err
	}

	args := []string{"build", "-t", tag, "--label", ManagedLabel, "--label", repoLabel + "=" + r.Repo}
	if r.Platform != "" {
		args = append(args, "--platform", r.Platform)
	}
	args = append(args, dir)

	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	start := time.Now()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	b := Built{Tag: tag, Duration: time.Since(start), Output: tail(string(out))}
	if err != nil {
		// The build log is the only thing that says why, and it is the
		// difference between "this repo needs a system package" and "Docker
		// is broken". Carry it rather than the exit status alone.
		return b, fmt.Errorf("%w: %s: %v\n%s", ErrBuild, tag, err, b.Output)
	}
	return b, nil
}

// PullAttempts and PullBackoff bound the retry. Deliberately small: a
// registry that is unreachable three times over half a minute is not going to
// answer on the fourth, and the stage failing cleanly is better than a
// scheduled run that holds the lock for an hour.
var (
	PullAttempts = 3
	PullBackoff  = 10 * time.Second
)

func pullBase(ctx context.Context, base, platform string) error {
	if base == "" {
		return fmt.Errorf("%w: recipe has no base image", ErrBuild)
	}
	if exists(ctx, base) {
		return nil
	}
	var last error
	for attempt := 0; attempt < PullAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(PullBackoff * time.Duration(attempt)):
			}
		}
		args := []string{"pull", "--quiet"}
		if platform != "" {
			args = append(args, "--platform", platform)
		}
		c, cancel := context.WithTimeout(ctx, pullTimeout)
		out, err := exec.CommandContext(c, "docker", append(args, base)...).CombinedOutput()
		cancel()
		if err == nil {
			return nil
		}
		last = fmt.Errorf("%w: pull %s: %v\n%s", ErrBuild, base, err, tail(string(out)))
	}
	return last
}

func exists(ctx context.Context, tag string) bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "image", "inspect", tag).Run() == nil
}

// List returns the pipeline's own images, newest first.
func List(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "image", "ls",
		"--filter", "label="+ManagedLabel, "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return nil, fmt.Errorf("%w: list: %v", ErrBuild, err)
	}
	var tags []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" && l != "<none>:<none>" {
			tags = append(tags, l)
		}
	}
	return tags, nil
}

// Prune removes the pipeline's images except the ones named in keep.
//
// It filters on ossp.managed=1 and never on anything else. This machine holds
// unrelated images and volumes belonging to other work, and a prune that
// reached them would be unrecoverable, so the filter is the safety property
// and TestPruneOnlyEverTouchesLabelledImages is what holds it.
func Prune(ctx context.Context, keep ...string) ([]string, error) {
	tags, err := List(ctx)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	var removed []string
	for _, t := range tags {
		if kept[t] {
			continue
		}
		c, cancel := context.WithTimeout(ctx, time.Minute)
		err := exec.CommandContext(c, "docker", "image", "rm", t).Run()
		cancel()
		if err == nil {
			removed = append(removed, t)
		}
	}
	sort.Strings(removed)
	return removed, nil
}

func tail(s string) string {
	if len(s) <= tailBytes {
		return s
	}
	s = s[len(s)-tailBytes:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return "... (truncated)\n" + s
}
