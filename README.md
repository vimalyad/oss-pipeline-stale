# oss-pipeline

A semi-automated system for contributing to open-source projects: it finds
issues worth working on, proposes them for human approval, implements the fix,
opens the pull request, and then services CI and maintainer review until the PR
is decided.

The interesting part is not the automation. It is everything that had to be
true before automation was safe.

---

## The constraints that shaped it

**One machine, several GitHub identities.** The host's ambient git and `gh`
configuration resolves to a different account than the one this pipeline
publishes under. A single commit pushed with the wrong identity into a public
repository is unfixable history. So identity is never *inherited* here — it is
asserted per clone, across four independent layers, and re-checked in-process
before every commit and push because layer four can be disarmed by the target
repository's own tooling (`npm install` rewrites `core.hooksPath`).

**An unattended agent must not be able to reach a public action.** The human
gate is a property of the state machine, not a flag: there is no transition
from `proposed` to `implementing`, so no combination of arguments gets an
unattended run to open a pull request. A test asserts the edge does not exist.

**Third-party code runs during verification.** Building and testing a target
repository executes code that project controls. That work is moving into
containers with no credentials, because a compromised dependency in any watched
project should find nothing worth taking.

---

## Design

```
cmd/pipeline/        entry point
internal/
  model/             candidate state machine, explicit transition table
  store/             one JSON file per candidate
  identity/          the four-layer identity boundary
  guard/             what may never reach a public PR
  ghx/  llm/         gh and claude CLIs, with retry and tool restrictions
  machine/           what this computer can actually verify
  score/  policy/    the gate, and what it reads
  cilog/             why a CI check failed
  notify/            reaching a human who is not at the machine
  repo/ toolchain/   clones, and how a project builds itself
```

Interfaces are declared where they are consumed, one method wide, so the
compiler checks the seams. An earlier version kept them in a central file that
nothing imported; four of its eight definitions had drifted to describe
functions that no longer existed, which is what a Protocol nobody checks decays
into.

Errors that indicate a bug in this system are distinct values from errors that
indicate a bad network, because a state-machine violation caught by the same
handler as a timeout is a lost outcome nobody notices.

State is JSON files rather than a database. When an unattended run wedges at
3am, being able to read the state with `cat` and correct it in a text editor has
repeatedly been the difference between a diagnosable failure and a mystery.

---

## Guards

Each of these exists because something got through.

| Guard | What it stops |
|---|---|
| Net shipped-file set | Agent scaffolding reaching a PR. The rule matched the path, but the scope was uncommitted changes only, so a file committed earlier was invisible. Untracked files count too — `git add -A` would ship them. |
| Forbidden PR language | An implementer's notes to its operator appearing in a public description, including first-person remarks about denied commands and a claim tests had not been run when they had. |
| Secret scan | Credentials in a diff. Private strings are read from gitignored configuration, never written here — a scanner that hardcodes the address it suppresses publishes it. |
| New top-level directory | A bug fix does not invent a directory. When one appears it is usually the implementer building scaffolding for itself. |
| Commit message rules | Describing what a deleted document was *about* rather than that it was removed. |
| Mirror-job suppression | Counting one CI failure twice. Some projects have a job whose entire body is `echo job failed && exit 1`. |
| Infrastructure classification | Escalating someone else's registry outage as though it were a defect in the diff. |

---

## Machine capability gating

Issues are only targeted if a fix can be verified on the hardware available.
Requirements are inferred from the thread and matched against a detected
profile; anything needing a GPU, operating system or cluster that is not
present is refused with the phrase from the thread that caused the refusal.

Apple GPU work is the one carve-out from "no target code on the host": Metal
cannot be virtualised into a Linux container, so verification for those runs on
the host, with credentials stripped, and is never eligible for autonomous
action.

---

## Status

The system runs daily and hourly under `launchd`, and as of 1 October 2026 both
jobs run the Go binary. The Python implementation is kept, unscheduled, as the
rollback target and as the second opinion the parity gates compare against.

Two parity gates guarded the port:

- `pipeline status` must be byte-identical between implementations.
- `pipeline rescore` must produce the same verdict *and the same reason string*
  for every stored candidate. It is offline and free, so it runs after any
  scoring change.

Reaching byte-level parity on the second one found a real defect rather than a
cosmetic one: the Go scorer was not falling back to cached repository facts, so
it rejected candidates the Python accepted. Comparing verdicts alone would have
missed it.

What the gates did *not* catch is worth recording, because it is the failure
that actually cost something. Running both implementations against one set of
state files makes the newer one's writes an input to the older one, and the Go
binary records two fields the Python dataclasses have no slot for. Python's
loaders raised `TypeError`; `should_reconsider` read that as "never seen
before" and `discover` rebuilt sixteen candidates from the search result,
discarding four human approvals and twelve human rejections. A second instance
one function away, in the repo-facts loader, took a whole day's discovery stage
down before it was found.

Both are fixed -- the loaders ignore unknown keys, and a file that exists but
cannot be read is never treated as absent -- but the lesson is about the shape
of the gate rather than the bug. Comparing two implementations' *output* says
nothing about what happens when they share a *writable* input. A shared-state
port needs a test that the older reader survives the newer writer, and there
wasn't one.

---

## Usage

```bash
pipeline doctor                  # state, identity, kill switch, history validity
pipeline machine                 # what this computer can verify
pipeline status                  # what is tracked and what is waiting
pipeline rescore                 # re-evaluate stored candidates, offline
pipeline check <file>            # screen text destined for a public comment
pipeline halt "reason"           # stop every scheduled stage

pipeline daily    [--execute]    # one full cycle, what launchd runs at 09:30
pipeline discover [--execute]    # sweep the watchlist
pipeline triage   [--execute]    # classify, harvest and brief what it found
pipeline propose                 # rank and write today's report
pipeline approve <slug>          # the human gate
pipeline reject  <slug> <reason>
pipeline retry   <slug> <reason> # put abandoned work back in the queue
pipeline implement <slug> [--execute]
pipeline watch    [--execute]    # one cycle over every open pull request
pipeline replies  [list|draft|post <slug> <n>]
```

Rollback is one line per job: point `ProgramArguments[0]` in the two plists
under `~/Library/LaunchAgents` back at `.venv/bin/pipeline` and reload them.

The implement stage needs Docker running. If it is not, the stage fails on the
image build *before* any state changes, so the candidate stays approved and the
next cycle retries it.

Dry run is the default. Nothing forks, commits, pushes or opens a pull request
without `--execute`.

```bash
go test ./...                    # Go
.venv/bin/python -m pytest -q    # Python
```

## Configuration

| File | Purpose |
|---|---|
| `config/profile.yaml` | Domains, weights, autonomy level per domain |
| `config/policy.yaml` | Caps, staleness thresholds, scoring bars |
| `config/watchlist.yaml` | Seed repositories |
| `config/exclusions.yaml` | Repositories never to target |
| `config/identity.env` | **Gitignored.** Logins, addresses, keychain coordinates |

Credentials live in the macOS Keychain and are read per process. No token is
ever written to disk or into a clone's git configuration.

## Disclosure

Parts of this pipeline are AI-assisted, and pull requests it opens disclose
that where a project's contribution policy asks for it. Projects that prohibit
AI-assisted contributions are excluded automatically by parsing their stated
policy. Commit messages carry no tooling attribution, which is a deliberate
choice about what belongs in a project's permanent history rather than an
attempt to obscure anything.
