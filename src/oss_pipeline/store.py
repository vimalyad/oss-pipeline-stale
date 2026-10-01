"""Candidate persistence: one JSON file each, so a stuck run can be debugged
with `cat` and unstuck with a text editor."""

from __future__ import annotations

import dataclasses
import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

from .identity import ROOT
from .models import Brief, Candidate, Contest, PRSignal, RepoFacts, Status

DIR = ROOT / "state" / "candidates"
CONTEXT = ROOT / "state" / "context"
REPOS = ROOT / "state" / "repos"


def path_for(slug: str) -> Path:
    return DIR / f"{slug}.json"


def save(cand: Candidate) -> Path:
    DIR.mkdir(parents=True, exist_ok=True)
    p = path_for(cand.slug)
    p.write_text(json.dumps(cand.to_dict(), indent=2, default=str))
    return p


def load(slug: str) -> Candidate:
    raw = json.loads(path_for(slug).read_text())
    return _hydrate(raw)


def _known(cls, raw: dict) -> dict:
    """Drop keys `cls` has no field for.

    The Go rewrite writes the same files and has fields this version does not:
    `topics` on RepoFacts, `took_over` on Candidate. Without this, every
    candidate the Go binary saved raised TypeError here -- and discover read
    that as "never seen", rebuilt the candidate from the search result and
    threw the status and history away. Four approvals and twelve rejections
    went that way in one scheduled sweep.

    Dropping them loses nothing this version can use, and it is the right
    default for a loader whose writer is ahead of it.
    """
    fields = {f.name for f in dataclasses.fields(cls)}
    return {k: v for k, v in raw.items() if k in fields}


def _hydrate(raw: dict) -> Candidate:
    raw = _known(Candidate, raw)
    raw["status"] = Status(raw["status"])
    raw["contest"] = Contest(raw["contest"]) if raw.get("contest") else None
    raw["pr_signal"] = PRSignal(**_known(PRSignal, raw["pr_signal"])) if raw.get("pr_signal") else None
    raw["brief"] = Brief(**_known(Brief, raw["brief"])) if raw.get("brief") else None
    raw["facts"] = RepoFacts(**_known(RepoFacts, raw["facts"])) if raw.get("facts") else None
    return Candidate(**raw)


def all_candidates() -> list[Candidate]:
    if not DIR.exists():
        return []
    out = []
    for p in sorted(DIR.glob("*.json")):
        try:
            out.append(_hydrate(json.loads(p.read_text())))
        except (json.JSONDecodeError, TypeError, ValueError) as exc:
            print(f"    warn: skipping unreadable {p.name}: {exc}")
    return out


def by_status(*statuses: Status) -> list[Candidate]:
    want = set(statuses)
    return [c for c in all_candidates() if c.status in want]


# Rejections fall into two kinds. Structural ones are facts about the repo or a
# human's decision and do not change on their own. Transient ones describe a
# moment in time -- someone was mid-PR, someone had just claimed it, the thread
# had not converged yet -- and all of those expire. Treating every rejection as
# permanent quietly discards the best candidates: the two strongest issues found
# on the first real sweep were both rejected as "claimed", and claims lapse.
TRANSIENT_REJECTIONS = (
    "contest=active_pr", "contest=claimed", "claimed by", "deferred:",
    "thread has not converged", "no maintainer acceptance",
    "no stated approach", "harvest/brief failed",
)
STRUCTURAL_REJECTIONS = (
    "bans AI", "no CONTRIBUTING", "requires a CLA", "manually excluded",
    "already touched by another of your accounts", "docs/typo-only",
    "no test suite", "unreceptive",
)


def is_transient(reason: str) -> bool:
    low = (reason or "").lower()
    if any(m.lower() in low for m in STRUCTURAL_REJECTIONS):
        return False
    return any(m.lower() in low for m in TRANSIENT_REJECTIONS)


def rejected_at(cand: Candidate) -> datetime | None:
    for entry in reversed(cand.history):
        if entry.get("to") != Status.REJECTED:
            continue
        try:
            return datetime.fromisoformat(entry["at"])
        except (ValueError, TypeError):
            continue        # hand-edited or synthetic marker; keep looking
    return None


def should_reconsider(slug: str, *, after_days: int) -> bool:
    """True when a tracked candidate deserves another look this sweep."""
    p = path_for(slug)
    if not p.exists():
        return True                       # never seen -- always consider
    try:
        cand = load(slug)
    except (json.JSONDecodeError, TypeError, ValueError) as exc:
        # A file that exists but cannot be read is not a candidate we have
        # never seen. Saying True here means discover builds a fresh one and
        # overwrites whatever was on disk, which is how a human approval gets
        # destroyed by a scheduled sweep. Refuse to touch it and say so.
        print(f"    warn: leaving {slug} alone -- cannot read it: {exc}")
        return False
    if cand.status is not Status.REJECTED:
        return False                      # live or terminal-by-success
    if cand.reject_reason.startswith("human rejection") or not is_transient(cand.reject_reason):
        return False
    when = rejected_at(cand) or datetime.fromtimestamp(p.stat().st_mtime, timezone.utc)
    return (datetime.now(timezone.utc) - when) >= timedelta(days=after_days)


def exists(slug: str) -> bool:
    return path_for(slug).exists()


def save_repo_facts(facts: RepoFacts) -> None:
    REPOS.mkdir(parents=True, exist_ok=True)
    (REPOS / f"{facts.repo.replace('/', '__')}.json").write_text(
        json.dumps(facts.__dict__, indent=2)
    )


def load_repo_facts(repo: str) -> RepoFacts | None:
    p = REPOS / f"{repo.replace('/', '__')}.json"
    if not p.exists():
        return None
    # Same forward-compatibility rule as _hydrate, and missed there once
    # already: the cached facts are written by whichever binary ran last, and
    # the Go one records `topics`. Unguarded, this raised TypeError out of
    # discover's phase B2 and took the whole stage down -- a full day with no
    # candidates and no proposals, from one extra key.
    return RepoFacts(**_known(RepoFacts, json.loads(p.read_text())))
