package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vimalyad/osspipeline/internal/audit"
	"github.com/vimalyad/osspipeline/internal/ghx"
	"github.com/vimalyad/osspipeline/internal/halt"
	"github.com/vimalyad/osspipeline/internal/identity"
	"github.com/vimalyad/osspipeline/internal/llm"
	"github.com/vimalyad/osspipeline/internal/model"
	"github.com/vimalyad/osspipeline/internal/replies"
	"github.com/vimalyad/osspipeline/internal/repo"
	"github.com/vimalyad/osspipeline/internal/store"
	"github.com/vimalyad/osspipeline/internal/text"
)

// repliesCmd handles the maintainer feedback the watcher queues.
//
// Three verbs, and the split between them is the point. `list` and `draft`
// prepare; only `post` publishes, and it takes one item at a time by name.
// There is deliberately no way to post everything: a reply goes out under the
// user's name to a person who is waiting for it, and the design asks exactly
// one question at a time -- "send this reply?", with the full text, Post or
// Skip. A --execute that swept the queue would answer that question on their
// behalf.
//
// v1 left five drafted replies unsent for days while a reviewer waited, and
// nothing said so. The drafting half of that is here; the telling half is
// notify, which watch now calls.
func repliesCmd(root string, args []string) int {
	verb := "list"
	if len(args) > 0 {
		verb = args[0]
		args = args[1:]
	}
	switch verb {
	case "list":
		return repliesList(root)
	case "draft":
		return repliesDraft(root, args)
	case "post":
		return repliesPost(root, args)
	default:
		fmt.Fprintf(os.Stderr, "unknown verb %q\n", verb)
		repliesUsage()
		return 2
	}
}

func repliesUsage() {
	fmt.Fprint(os.Stderr, `usage: pipeline replies [list | draft [--execute] | post <slug> <n>]

  list                  every queued item and the state of its draft
  draft [--execute]     write a reply for anything queued without one
  post <slug> <n>       publish one drafted reply, by the index list shows

There is no "post everything". A reply goes out under your name to someone
waiting for it, so each one is its own decision.
`)
}

// queueItem is one pending reply with enough context to decide about it.
type queueItem struct {
	cand  *model.Candidate
	index int
}

func pendingQueue(st *store.Store) []queueItem {
	var out []queueItem
	for _, c := range st.ByStatus(model.OpenStatuses...) {
		for _, i := range replies.Pending(c) {
			out = append(out, queueItem{cand: c, index: i})
		}
	}
	return out
}

func repliesList(root string) int {
	q := pendingQueue(store.New(root))
	if len(q) == 0 {
		fmt.Println("no replies queued")
		return 0
	}
	for _, it := range q {
		item := it.cand.QueuedReplies[it.index]
		fmt.Printf("\n%s  [%d]  %s#%d\n", it.cand.Slug(), it.index,
			it.cand.Repo, it.cand.Issue)
		fmt.Printf("  from %s (%s): %s\n", field(item, "author"), field(item, "cls"),
			text.FirstLine(field(item, "why"), 160))
		if body := field(item, "body"); body != "" {
			fmt.Printf("  they said: %s\n", text.FirstLine(body, 100))
		}
		draft := field(item, "draft")
		switch ok, why := replies.Usable(item); {
		case ok:
			fmt.Printf("  DRAFT (post with `pipeline replies post %s %d`):\n",
				it.cand.Slug(), it.index)
			for _, line := range strings.Split(draft, "\n") {
				fmt.Println("    " + line)
			}
		default:
			fmt.Printf("  not sendable: %s\n", why)
		}
	}
	fmt.Printf("\n%d item(s) queued\n", len(q))
	return 0
}

func repliesDraft(root string, args []string) int {
	execute := false
	for _, a := range args {
		if a == "--execute" {
			execute = true
		} else {
			fmt.Fprintf(os.Stderr, "unknown argument %q\n", a)
			return 2
		}
	}
	// Drafting publishes nothing, but it costs a model call and writes to the
	// candidate, so a halt stops it too: a halted pipeline should be doing
	// nothing at all, not quietly spending.
	if reason, active := halt.New(root).Active(); active {
		fmt.Fprintf(os.Stderr, "HALT is set (%s); not drafting\n", reason)
		return 1
	}

	id, err := identity.Load(filepath.Join(root, "config", "identity.env"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	token, err := identity.Token(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	env := identity.Env(id, token)
	brain := llm.New(env)
	rm := &repo.Manager{Root: root, ID: id, Env: env, Log: say}
	st := store.New(root)

	ctx := context.Background()
	done := 0
	for _, c := range st.ByStatus(model.OpenStatuses...) {
		if len(replies.Pending(c)) == 0 {
			continue
		}
		rc, err := replyContext(ctx, rm, c)
		if err != nil {
			// Without the diff a draft is written from commit subjects alone,
			// and one of those once announced three fixes as "not yet done"
			// when the diff already contained all three. Three false
			// statements to a maintainer is worse than no reply.
			fmt.Fprintf(os.Stderr, "%s: %v\n", c.Slug(), err)
			continue
		}
		n, err := replies.DraftAll(ctx, brain, c, rc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", c.Slug(), err)
		}
		if n == 0 {
			continue
		}
		done += n
		fmt.Printf("%s: %d draft(s) written\n", c.Slug(), n)
		if execute {
			if _, err := st.Save(c); err != nil {
				fmt.Fprintf(os.Stderr, "save %s: %v\n", c.Slug(), err)
			}
		}
	}
	if done == 0 {
		fmt.Println("nothing to draft")
		return 0
	}
	if !execute {
		fmt.Println("\n[dry run] the drafts were not saved; run with --execute to keep them")
		return 0
	}
	fmt.Printf("\n%d draft(s) ready. `pipeline replies list` to read them.\n", done)
	return 0
}

// replyContext reads the branch the reply is about. The clone has to exist
// already: cloning here would make a listing command do minutes of work, and
// the branch is what implement left behind.
func replyContext(ctx context.Context, rm *repo.Manager, c *model.Candidate) (replies.Context, error) {
	dir := rm.Dir(c.Repo)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return replies.Context{}, fmt.Errorf("no clone at %s; the diff is not optional for a draft", dir)
	}
	base := rm.DefaultBranch(ctx, dir)
	commits, _ := rm.Git(ctx, dir, "log", "--oneline", "origin/"+base+"..HEAD")
	diff, err := rm.Git(ctx, dir, "diff", "origin/"+base+"...HEAD")
	if err != nil {
		return replies.Context{}, fmt.Errorf("reading the branch diff: %v", err)
	}
	if strings.TrimSpace(diff) == "" {
		return replies.Context{}, errors.New("the branch has no diff against its base")
	}
	return replies.Context{Commits: commits, Diff: diff}, nil
}

func repliesPost(root string, args []string) int {
	if len(args) != 2 {
		repliesUsage()
		return 2
	}
	slug := args[0]
	idx, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%q is not an index; `pipeline replies list` shows them\n", args[1])
		return 2
	}
	if reason, active := halt.New(root).Active(); active {
		fmt.Fprintf(os.Stderr, "HALT is set (%s); not posting\n", reason)
		return 1
	}

	st := store.New(root)
	c, err := st.Load(slug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	id, err := identity.Load(filepath.Join(root, "config", "identity.env"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	token, err := identity.Token(id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	gh := ghx.New(identity.Env(id, token))

	url, err := replies.Post(context.Background(), prCommenter{gh}, c, idx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		// Post marks the item when the guard rejected its text, and that has
		// to reach disk or the next run re-checks text nobody looked at.
		if errors.Is(err, replies.ErrNoDraft) {
			if _, serr := st.Save(c); serr != nil {
				fmt.Fprintln(os.Stderr, "save:", serr)
			}
		}
		return 1
	}
	if _, err := st.Save(c); err != nil {
		// The comment is public. Saying so is the only thing left to do.
		fmt.Fprintln(os.Stderr, "the reply WAS posted at", url, "but saving failed:", err)
		return 1
	}
	_ = audit.New(root).Record("reply_posted", c.Slug(), url)
	fmt.Println("posted", url)
	return 0
}

// field reads a string out of a queued item. The items are map[string]any
// because that is their on-disk shape, hand-editable by design.
func field(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}
