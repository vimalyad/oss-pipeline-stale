"""Unit tests for the pipeline stages. No network, no LLM, no tokens."""

from __future__ import annotations

import dataclasses
import json
import pathlib
from datetime import datetime, timedelta, timezone

import pytest

from oss_pipeline import brief, policy, submit
from oss_pipeline.contest import _classify_one
from oss_pipeline.models import (Candidate, Contest, PRSignal, Status,
                                 TransitionError, transition)
from oss_pipeline.score import maintainer_accepted


def cand(**kw) -> Candidate:
    base = dict(repo="acme/widget", issue=1, title="a bug", url="u")
    return Candidate(**{**base, **kw})


# --------------------------------------------------------------- state machine
def test_happy_path_transitions():
    c = cand()
    for s in (Status.SCORED, Status.PROPOSED, Status.APPROVED, Status.IMPLEMENTING,
              Status.IMPLEMENTED, Status.PUSHED, Status.PR_OPEN, Status.MERGED):
        transition(c, s)
    assert c.status is Status.MERGED
    assert len(c.history) == 8


def test_human_gate_is_structurally_unreachable():
    """The single most important property: no path to code without approval."""
    for start in (Status.DISCOVERED, Status.SCORED, Status.PROPOSED):
        c = cand(status=start)
        with pytest.raises(TransitionError):
            transition(c, Status.IMPLEMENTING)


def test_terminal_states_are_terminal():
    for terminal in (Status.MERGED, Status.REJECTED, Status.ABANDONED):
        c = cand(status=terminal)
        with pytest.raises(TransitionError):
            transition(c, Status.PR_OPEN)


# ----------------------------------------------------------------- staleness
def sig(**kw) -> PRSignal:
    base = dict(number=1, url="u", author="someone", is_draft=False,
                days_since_commit=None, days_since_author_comment=None,
                days_since_changes_requested=None, has_stale_label=False,
                checks_failing=False)
    return PRSignal(**{**base, **kw})


@pytest.mark.parametrize("days", [0, 1, 15, 29])
def test_recent_author_activity_is_always_active(days):
    verdict, _ = _classify_one(sig(days_since_commit=days))
    assert verdict is Contest.ACTIVE_PR


def test_long_silence_is_stale():
    verdict, reasons = _classify_one(sig(days_since_commit=120))
    assert verdict is Contest.STALE_PR
    assert "no commit for 120d" in reasons[0]


def test_stale_label_marks_stale():
    verdict, reasons = _classify_one(sig(days_since_commit=45, has_stale_label=True))
    assert verdict is Contest.STALE_PR


def test_ambiguous_gap_defaults_to_active():
    """Between the windows with no positive staleness signal: do not contest."""
    verdict, reasons = _classify_one(sig(days_since_commit=45))
    assert verdict is Contest.ACTIVE_PR
    assert "conservative" in reasons[0]


def test_answered_changes_request_is_not_stale():
    verdict, _ = _classify_one(sig(
        days_since_commit=50, days_since_changes_requested=40,
        days_since_author_comment=35,       # replied AFTER the request
    ))
    assert verdict is Contest.ACTIVE_PR


def test_unanswered_changes_request_is_stale():
    verdict, reasons = _classify_one(sig(
        days_since_commit=50, days_since_changes_requested=40,
        days_since_author_comment=45,       # went quiet BEFORE the request
    ))
    assert verdict is Contest.STALE_PR
    assert "unanswered" in " ".join(reasons)


# ------------------------------------------------------- maintainer acceptance
def test_triage_gated_label_is_acceptance():
    ok, why = maintainer_accepted(cand(labels=["help wanted", "bug"]))
    assert ok and "help wanted" in why


def test_bug_label_alone_is_not_acceptance():
    """Issue templates auto-apply `bug`, so it proves nothing about triage."""
    ok, _ = maintainer_accepted(cand(labels=["bug"]))
    assert not ok


def test_untriaged_label_vetoes_acceptance():
    ok, why = maintainer_accepted(cand(labels=["help wanted", "triage/pending"]))
    assert not ok and "untriaged" in why


def test_maintainer_statement_is_acceptance_without_a_label():
    c = cand(labels=["bug"])
    c.brief = brief.Brief(maintainer_desired_approach="do it this way",
                          approach_author_association="MEMBER")
    ok, why = maintainer_accepted(c)
    assert ok and "stated an approach" in why


# ------------------------------------------------------------------ caps
def _pr_opened(c: Candidate, when: datetime) -> Candidate:
    c.status = Status.PR_OPEN
    c.history = [{"at": when.isoformat(), "from": "pushed", "to": Status.PR_OPEN, "note": ""}]
    return c


def test_max_open_per_repo():
    now = datetime.now(timezone.utc)
    existing = _pr_opened(cand(issue=9), now)
    reasons = policy.check_caps([existing], cand(issue=10))
    assert any("already have an open PR" in r for r in reasons)


def test_daily_cap():
    now = datetime.now(timezone.utc)
    opened = [_pr_opened(cand(repo=f"o{i}/r", issue=i), now) for i in range(2)]
    reasons = policy.check_caps(opened, cand(repo="fresh/repo", issue=99))
    assert any("opened today" in r for r in reasons)


def test_org_cooldown():
    recent = datetime.now(timezone.utc) - timedelta(days=1)
    existing = _pr_opened(cand(repo="acme/other", issue=5), recent)
    reasons = policy.check_caps([existing], cand(repo="acme/widget", issue=6))
    assert any("cooldown" in r for r in reasons)


def test_caps_clear_when_nothing_open():
    assert policy.check_caps([], cand()) == []


# ------------------------------------------------------------- secret scanning
@pytest.mark.parametrize("blob,expect", [
    ("token = ghp_" + "a" * 30, "GitHub token"),
    ("AKIA" + "B" * 16, "AWS access key"),
    ("-----BEGIN RSA PRIVATE KEY-----", "private key"),
])
def test_secrets_are_caught(blob, expect):
    assert expect in submit.scan_secrets(blob)


def test_clean_diff_scans_clean():
    assert submit.scan_secrets("def add(a, b):\n    return a + b\n") == []


# ------------------------------------------------------------ brief JSON parse
@pytest.mark.parametrize("raw", [
    '{"claimed_by": "bob"}',
    '```json\n{"claimed_by": "bob"}\n```',
    'Here you go:\n{"claimed_by": "bob"}\nhope that helps',
])
def test_json_extraction_tolerates_wrapping(raw):
    assert brief._extract_json(raw)["claimed_by"] == "bob"


def test_json_extraction_fails_loudly_on_prose():
    with pytest.raises(ValueError):
        brief._extract_json("I could not find anything useful.")


def test_bot_detection():
    assert brief._is_bot("coderabbitai[bot]")
    assert brief._is_bot("github-actions")
    assert not brief._is_bot("sharkdp")


# ------------------------------------------------- regressions from the E2E run
@pytest.mark.parametrize("path", [
    "chunker/__pycache__/__init__.cpython-313.pyc",
    "tests/__pycache__/test_x.cpython-313-pytest-9.1.1.pyc",
    ".pytest_cache/README.md",
    "node_modules/left-pad/index.js",
    ".DS_Store",
])
def test_build_artefacts_are_rejected(path):
    """These were staged during the first live run and would have shipped."""
    assert submit.JUNK.search(path), path


@pytest.mark.parametrize("path", ["chunker/__init__.py", "tests/test_chunker.py",
                                  "src/pycache_helper.py", "docs/notes.pycon.md"])
def test_real_source_files_are_not_artefacts(path):
    assert not submit.JUNK.search(path), path


def test_our_own_lockfile_is_flagged():
    """`uv run` creates uv.lock; our test tooling must not leak into the diff."""
    assert "uv.lock" in submit.OUR_TOOLING


# ---------------------------------------------------- rejection reconsideration
@pytest.mark.parametrize("reason", [
    "contest=active_pr (author active 3d ago)",
    "claimed by @someone -- respect the claim",
    "thread has not converged: 2 open question(s)",
    "deferred: exceeded max_harvest=14 this run",
    "no maintainer acceptance (no triage-gated label)",
])
def test_transient_rejections_come_back(reason):
    from oss_pipeline import store
    assert store.is_transient(reason), reason


@pytest.mark.parametrize("reason", [
    "repo bans AI-assisted PRs: 'no AI'",
    "no CONTRIBUTING.md",
    "repo requires a CLA (sign it manually, then re-score)",
    "acme/widget is manually excluded",
    "acme/widget already touched by another of your accounts (otheracct)",
    "looks docs/typo-only -- noise in a mature repo",
])
def test_structural_rejections_stay_rejected(reason):
    from oss_pipeline import store
    assert not store.is_transient(reason), reason


def test_structural_beats_transient_when_both_present():
    """A candidate both claimed AND in a CLA repo must not come back."""
    from oss_pipeline import store
    assert not store.is_transient("claimed by @x; repo requires a CLA")


# ------------------------------------------------ targeted test selection
def test_targeted_tests_prefers_touched_test_files(tmp_path):
    """Regression: the first real run abandoned a good patch because it ran the
    whole of kornia's suite in an environment missing the project's plugins."""
    from oss_pipeline import toolchain
    (tmp_path / "pyproject.toml").write_text(
        '[project]\nname="x"\n[project.optional-dependencies]\ndev=["pytest-timeout"]\n'
    )
    (tmp_path / "tests").mkdir()
    cmds, why = toolchain.targeted_tests(
        tmp_path, ["src/mod.py", "tests/test_mod.py", "CHANGELOG.md"]
    )
    assert len(cmds) == 1
    assert "tests/test_mod.py" in cmds[0], cmds
    assert "--extra dev" in cmds[0], "project test extras must be installed"
    assert "targeted" in why


def test_python_extras_are_discovered(tmp_path):
    from oss_pipeline import toolchain
    (tmp_path / "pyproject.toml").write_text(
        '[project]\nname="x"\n'
        '[project.optional-dependencies]\ndev=["a"]\ndocs=["b"]\ntesting=["c"]\n'
    )
    assert set(toolchain._python_extras(tmp_path)) == {"dev", "testing"}, "docs is not a test extra"


def test_go_targets_changed_packages_only(tmp_path):
    from oss_pipeline import toolchain
    (tmp_path / "go.mod").write_text("module x\n")
    cmds, why = toolchain.targeted_tests(tmp_path, ["pkg/cmd/issue/create.go"])
    assert cmds == ["go test ./pkg/cmd/issue"], cmds


def test_falls_back_when_no_tests_identified(tmp_path):
    from oss_pipeline import toolchain
    (tmp_path / "pyproject.toml").write_text('[project]\nname="x"\n')
    cmds, why = toolchain.targeted_tests(tmp_path, ["README.md"])
    assert "no test files identified" in why


def test_pr_title_prefers_commit_subject(tmp_path):
    """Regression: kornia#4455 opened as 'fix: MPS: torch 2.14 ... fail at >= 8192
    input' -- the issue title, doubled prefix, truncated, describing the bug."""
    import subprocess as sp
    from oss_pipeline import submit
    repo = tmp_path / "r"; repo.mkdir()
    sp.run(["git", "init", "-q", str(repo)], check=True)
    (repo / "f.txt").write_text("x")
    sp.run(["git", "-C", str(repo), "add", "-A"], check=True)
    sp.run(["git", "-C", str(repo), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "fix: fall back to CPU for MPS SVD batches"], check=True)
    subject = sp.run(["git", "-C", str(repo), "log", "-1", "--format=%s"],
                     capture_output=True, text=True).stdout.strip()
    assert subject == "fix: fall back to CPU for MPS SVD batches"
    assert not subject.startswith("fix: fix:")


# ------------------------------------------------------- PR body leak filter
@pytest.mark.parametrize("body", [
    "I could not execute the verification commands, so this is statically reviewed only.",
    "Every uv invocation in this session was denied by the harness sandbox.",
    "This command requires approval, so the change was not run.",
    "Generated with Claude Code.",
    "I guessed #4394 as the PR number.",
    "An AI assistant prepared this patch.",
])
def test_agent_transcript_never_reaches_a_pr_body(body):
    """Regression: kornia#4455 published the implementer's internal notes."""
    assert submit.BODY_FORBIDDEN.search(body), body


@pytest.mark.parametrize("body", [
    "torch 2.14's MPS linalg.svd fails to build its Metal pipeline state object.",
    "**Change.** _torch_svd_cast now decomposes oversized MPS inputs on the CPU.",
    "**Verification.** tests/core/test_helpers.py passes (37 tests).",
    "The maintainer suggested this approach in the issue thread; chunking was ruled out.",
])
def test_legitimate_pr_prose_is_not_blocked(body):
    assert not submit.BODY_FORBIDDEN.search(body), body


# --------------------------------------------- dependent-module test discovery
def _git_repo(tmp_path):
    import subprocess as sp
    r = tmp_path / "r"; (r / "pkg").mkdir(parents=True); (r / "tests").mkdir()
    sp.run(["git", "init", "-q", str(r)], check=True)
    return r


def test_symbols_come_from_the_diff_not_the_whole_file(tmp_path):
    """Regression: kornia#4455. Scanning the file returned 27 symbols and the
    alphabetical cut dropped `_torch_svd_cast`, the only one that changed."""
    import subprocess as sp
    from oss_pipeline import toolchain
    r = _git_repo(tmp_path)
    src = r / "pkg" / "utils.py"
    src.write_text("\n".join(f"def aaa_{i}():\n    return {i}\n" for i in range(20))
                   + "\ndef zzz_target():\n    return 1\n")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "init"], check=True)
    src.write_text(src.read_text().replace("def zzz_target():\n    return 1",
                                           "def zzz_target():\n    return 2"))
    syms = toolchain._changed_symbols(r, ["pkg/utils.py"])
    assert syms == ["zzz_target"], f"expected only the edited symbol, got {syms}"


def test_dependent_tests_follow_callers(tmp_path):
    """A change to utils.py must pull in tests for the module that CALLS it."""
    import subprocess as sp
    from oss_pipeline import toolchain
    r = _git_repo(tmp_path)
    (r / "pkg" / "utils.py").write_text("def compute_thing():\n    return 1\n")
    (r / "pkg" / "consumer.py").write_text(
        "from .utils import compute_thing\n\ndef go():\n    return compute_thing()\n")
    (r / "tests" / "test_consumer.py").write_text("def test_go():\n    pass\n")
    (r / "tests" / "test_utils.py").write_text("def test_u():\n    pass\n")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "init"], check=True)
    (r / "pkg" / "utils.py").write_text("def compute_thing():\n    return 2\n")

    deps = toolchain.dependent_tests(r, ["pkg/utils.py"])
    assert "tests/test_consumer.py" in deps, deps


def test_compiled_artefacts_are_never_test_targets(tmp_path):
    from oss_pipeline import toolchain
    r = _git_repo(tmp_path)
    (r / "pkg" / "mod.py").write_text("x = 1\n")
    (r / "tests" / "test_mod.py").write_text("def test(): pass\n")
    (r / "tests" / "__pycache__").mkdir()
    (r / "tests" / "__pycache__" / "test_mod.cpython-313.pyc").write_bytes(b"\x00")
    found = toolchain.mirrored_tests(r, ["pkg/mod.py"])
    assert "tests/test_mod.py" in found
    assert not any(".pyc" in f or "__pycache__" in f for f in found), found


def test_sync_refuses_when_local_has_unpushed_commits(tmp_path, monkeypatch):
    """Never reset away local work; and never force over a maintainer's commits."""
    import subprocess as sp
    from oss_pipeline import watch
    from oss_pipeline.models import Candidate

    bare = tmp_path / "fork.git"; sp.run(["git", "init", "-q", "--bare", str(bare)], check=True)
    r = tmp_path / "r"; sp.run(["git", "init", "-q", "-b", "topic", str(r)], check=True)
    sp.run(["git", "-C", str(r), "remote", "add", "fork", str(bare)], check=True)
    (r / "f.txt").write_text("a")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "one"], check=True)
    sp.run(["git", "-C", str(r), "push", "-q", "fork", "topic"], check=True)
    # a local commit the fork has not seen
    (r / "f.txt").write_text("b")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "local only"], check=True)

    monkeypatch.setattr(watch, "pipeline_env", lambda: {"PATH": "/usr/bin:/bin", "HOME": str(tmp_path)})
    c = Candidate(repo="a/b", issue=1, title="t", url="u", branch="topic")
    with pytest.raises(watch.BranchDiverged):
        watch.sync_with_remote(c, r)


# --------------------------------------------------- agent scaffolding leakage
@pytest.mark.parametrize("path", [
    ".agents/skills/kornia-developer/SKILL.md",
    ".claude/settings.json",
    ".cursor/rules.md",
    "docs/SKILL.md",
    "CLAUDE.md",
    ".github/copilot-instructions/x.md",
])
def test_agent_scaffolding_is_rejected(path):
    """Regression: an auto-fix committed .agents/skills/.../SKILL.md into a real
    kornia PR -- 92 lines naming an agent tool."""
    assert submit.AGENT_ARTEFACTS.search(path), path


@pytest.mark.parametrize("path", [
    "kornia/core/utils.py", "tests/core/test_helpers.py",
    "changelog.d/4455.fixed.md", "docs/source/agents_guide.rst",
])
def test_real_project_files_are_not_flagged_as_scaffolding(path):
    assert not submit.AGENT_ARTEFACTS.search(path), path


def test_old_issues_are_penalised_not_rejected():
    """datasets#2267 was opened in 2021; its stated approach may predate the code."""
    from datetime import datetime, timedelta, timezone
    from oss_pipeline import score
    old = (datetime.now(timezone.utc) - timedelta(days=5 * 365)).isoformat()
    c = cand(labels=["good first issue"], issue_created_at=old)
    c.brief = brief.Brief(maintainer_desired_approach="do x",
                          approach_author_association="MEMBER")
    c.facts = __import__("oss_pipeline.models", fromlist=["m"]).RepoFacts(
        repo=c.repo, has_contributing=True, has_tests=True,
        merged_first_time_pr_90d=True)
    ok, fails = score.score(c, touched=set())
    assert not any("opened" in f for f in fails), "age must not be a hard reject"
    assert any("5y ago" in p for p in c.soft_penalties), c.soft_penalties


def test_missing_toolchain_is_detected(monkeypatch):
    """Regression: cli/cli#14386 wrote a correct patch then died on
    'go: command not found' -- after all the expensive work."""
    from oss_pipeline import toolchain
    monkeypatch.setattr(toolchain.shutil, "which", lambda b, **k: None)
    assert toolchain.missing_toolchain("Go") == "go"
    assert toolchain.missing_toolchain("Rust") == "cargo"
    assert toolchain.missing_toolchain("TypeScript") == "npm"


def test_present_toolchain_is_not_a_blocker(monkeypatch):
    from oss_pipeline import toolchain
    monkeypatch.setattr(toolchain.shutil, "which", lambda b, **k: "/usr/bin/" + b)
    assert toolchain.missing_toolchain("Go") is None


def test_unknown_language_is_not_blocked():
    from oss_pipeline import toolchain
    assert toolchain.missing_toolchain("Haskell") is None
    assert toolchain.missing_toolchain("") is None


def test_lock_blocks_a_second_mutating_run(monkeypatch, tmp_path):
    """The daily job and an hourly check must never patch the same branch at once."""
    from oss_pipeline import lock
    monkeypatch.setattr(lock, "LOCK", tmp_path / "pipeline.lock")
    with lock.exclusive("implement"):
        assert lock.holder()["what"] == "implement"
        with pytest.raises(lock.Busy):
            with lock.exclusive("watch"):
                pass
    assert lock.holder() is None, "lock must be released"


def test_lock_from_a_dead_process_is_reclaimed(monkeypatch, tmp_path):
    import json, time
    from oss_pipeline import lock
    monkeypatch.setattr(lock, "LOCK", tmp_path / "pipeline.lock")
    (tmp_path / "pipeline.lock").write_text(
        json.dumps({"pid": 999999, "what": "crashed", "at": time.time()}))
    assert lock.holder() is None, "a dead holder must not wedge the pipeline"
    with lock.exclusive("watch") as got:
        assert got


# ------------------------------------------------- project eligibility rules
def _facts(**kw):
    from oss_pipeline.models import RepoFacts
    base = dict(repo="cli/cli", has_contributing=True, has_tests=True,
                merged_first_time_pr_90d=True)
    return RepoFacts(**{**base, **kw})


def test_missing_required_issue_label_is_rejected():
    """Regression: cli/cli#14448 was closed because issue #14386 lacked
    `help wanted`, which their CONTRIBUTING.md requires in plain English."""
    from oss_pipeline import score
    c = cand(repo="cli/cli", labels=["bug", "priority-2", "gh-issue"])
    c.brief = brief.Brief(maintainer_desired_approach="do x",
                          approach_author_association="MEMBER")
    c.facts = _facts(required_issue_labels=["help wanted"])
    ok, fails = score.score(c, touched=set())
    assert not ok
    assert any("help wanted" in f for f in fails), fails


def test_required_label_present_passes():
    from oss_pipeline import score
    c = cand(repo="cli/cli", labels=["help wanted", "bug"])
    c.brief = brief.Brief(maintainer_desired_approach="do x",
                          approach_author_association="MEMBER")
    c.facts = _facts(required_issue_labels=["help wanted"])
    ok, fails = score.score(c, touched=set())
    assert not any("help wanted" in f for f in fails), fails


def test_forbidden_issue_label_is_rejected():
    """cli/cli: 'Do not open pull requests for any issue marked core'."""
    from oss_pipeline import score
    c = cand(repo="cli/cli", labels=["help wanted", "core"])
    c.brief = brief.Brief(maintainer_desired_approach="do x",
                          approach_author_association="MEMBER")
    c.facts = _facts(required_issue_labels=["help wanted"],
                     forbidden_issue_labels=["core"])
    ok, fails = score.score(c, touched=set())
    assert not ok
    assert any("core" in f for f in fails), fails


def test_discovery_includes_repo_required_labels(tmp_path, monkeypatch):
    """eslint requires `accepted`, which is not in the generic label list, so
    without this the only issues findable there are ones it will not accept."""
    from oss_pipeline import discover, store
    from oss_pipeline.models import RepoFacts
    monkeypatch.setattr(store, "REPOS", tmp_path)
    store.save_repo_facts(RepoFacts(repo="eslint/eslint",
                                    required_issue_labels=["accepted"]))
    q = discover._query_for("eslint/eslint", ["good first issue", "bug"])
    assert '"accepted"' in q, q
    assert '"good first issue"' in q, q


def test_discovery_does_not_duplicate_a_label(tmp_path, monkeypatch):
    from oss_pipeline import discover, store
    from oss_pipeline.models import RepoFacts
    monkeypatch.setattr(store, "REPOS", tmp_path)
    store.save_repo_facts(RepoFacts(repo="cli/cli",
                                    required_issue_labels=["help wanted"]))
    q = discover._query_for("cli/cli", ["help wanted", "bug"])
    assert q.count('"help wanted"') == 1, q


def test_gist_scope_detected_from_header(monkeypatch):
    """GitHub 404s (not 403s) a gist POST when the scope is absent, so the
    scopes header is the only reliable signal."""
    from oss_pipeline import publish
    monkeypatch.setattr(publish, "_gh",
                        lambda a, s=None: (0, "x-oauth-scopes: public_repo, gist\n", ""))
    assert publish.has_gist_scope()
    monkeypatch.setattr(publish, "_gh",
                        lambda a, s=None: (0, "x-oauth-scopes: public_repo\n", ""))
    assert not publish.has_gist_scope()


def test_publish_raises_actionable_error_without_scope(monkeypatch):
    from oss_pipeline import publish
    monkeypatch.setattr(publish, "has_gist_scope", lambda: False)
    with pytest.raises(publish.ScopeMissing):
        publish.publish()


def test_queued_work_is_not_listed_as_needing_you(tmp_path, monkeypatch):
    """An approved, unblocked candidate is waiting on the scheduler, not on the
    author. Listing it under 'needs you' teaches the reader to skip the section."""
    from oss_pipeline import report, store
    from oss_pipeline.models import Status

    c = cand(repo="pytorch/vision", issue=598, status=Status.APPROVED)
    monkeypatch.setattr(store, "by_status",
                        lambda *s: [c] if Status.APPROVED in s else [])
    monkeypatch.setattr(store, "all_candidates", lambda: [c])
    monkeypatch.setattr(report, "_pr_state", lambda x: {})
    text = report.build()
    assert "Queued — no action needed" in text
    if "## Needs you" in text:
        section = text.split("## Needs you")[1].split("##")[0]
        assert "pytorch/vision#598" not in section, section


# --------------------------------------------------- transient-failure handling
def test_connection_errors_are_retried(monkeypatch):
    """A dropped connection took down a whole unattended daily run."""
    import subprocess as sp
    from oss_pipeline import ghapi
    calls = {"n": 0}

    class R:
        def __init__(self, rc, err): self.returncode, self.stderr, self.stdout = rc, err, "ok"

    def fake(*a, **k):
        calls["n"] += 1
        if calls["n"] < 3:
            return R(1, "error connecting to api.github.com")
        return R(0, "")

    monkeypatch.setattr(sp, "run", fake)
    monkeypatch.setattr(ghapi.time, "sleep", lambda s: None)
    monkeypatch.setattr(ghapi, "pipeline_env", dict)
    assert ghapi._run(["api", "user"]) == "ok"
    assert calls["n"] == 3, "should have retried twice before succeeding"


def test_unreachable_is_not_reported_as_missing_scope(monkeypatch):
    """Reporting 'add the gist scope' for a dead network sends you to the wrong page."""
    from oss_pipeline import publish
    monkeypatch.setattr(publish, "_gh",
                        lambda a, s=None: (1, "", "error connecting to api.github.com"))
    with pytest.raises(publish.Unreachable):
        publish.has_gist_scope()


def test_daily_stage_failure_does_not_stop_later_stages(capsys):
    from oss_pipeline import run
    run._stage("one", lambda: (_ for _ in ()).throw(RuntimeError("boom")))
    run._stage("two", lambda: ["ran anyway"])
    out = capsys.readouterr().out
    assert "STAGE FAILED" in out and "ran anyway" in out


def test_broken_clone_is_detected(tmp_path):
    """Regression: a clone made during a network outage reported all 708 tracked
    files as modified, and pytest was handed a LICENSE file as a test target."""
    import subprocess as sp
    from oss_pipeline import repo
    r = tmp_path / "r"; r.mkdir()
    sp.run(["git", "init", "-q", str(r)], check=True)
    for i in range(20):
        (r / f"f{i}.txt").write_text("original")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "init"], check=True)

    ok, why = repo.clone_is_sane(r)
    assert ok, why

    for i in range(20):                       # everything differs => broken
        (r / f"f{i}.txt").write_text("changed")
    ok, why = repo.clone_is_sane(r)
    assert not ok and "clone is broken" in why, why


def test_small_patch_is_sane(tmp_path):
    import subprocess as sp
    from oss_pipeline import repo
    r = tmp_path / "r"; r.mkdir()
    sp.run(["git", "init", "-q", str(r)], check=True)
    for i in range(20):
        (r / f"f{i}.txt").write_text("original")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "init"], check=True)
    (r / "f0.txt").write_text("patched")      # one file: a normal fix
    ok, why = repo.clone_is_sane(r)
    assert ok, why


# ------------------------------------------------------- what counts as a test
@pytest.mark.parametrize("path", [
    "test/assets/damaged_jpeg/TensorFlow-LICENSE",
    "test/assets/damaged_jpeg/bad_huffman.jpg",
    "test/assets/fakedata/logos/rgb_pytorch.png",
    "tests/README.md",
    "test/data/sample.json",
])
def test_fixtures_under_a_test_dir_are_not_test_files(path):
    """Regression: pytorch/vision keeps binary fixtures in test/assets/, and a
    directory-based filter handed pytest a LICENSE file and a JPEG."""
    from oss_pipeline import toolchain
    assert not toolchain.is_test_file(path), path


@pytest.mark.parametrize("path", [
    "test/test_transforms.py", "tests/core/test_helpers.py",
    "src/thing_test.py", "tests/conftest.py",
    "src/app.test.ts", "src/app.spec.js",
    "pkg/cmd/create_test.go",
])
def test_real_test_files_are_recognised(path):
    from oss_pipeline import toolchain
    assert toolchain.is_test_file(path), path


def test_target_list_is_capped(tmp_path, monkeypatch):
    """An old-PR takeover merges months of upstream work; 200 targets is not
    'targeted', and CI covers the rest."""
    from oss_pipeline import toolchain
    (tmp_path / "pyproject.toml").write_text('[project]\nname="x"\n')
    many = [f"tests/test_mod{i}.py" for i in range(200)]
    monkeypatch.setattr(toolchain, "dependent_tests", lambda *a, **k: [])
    cmds, why = toolchain.targeted_tests(tmp_path, many)
    assert cmds[0].count("tests/test_mod") == toolchain.MAX_TEST_TARGETS
    assert "trimmed" in why, why


def test_ancient_pr_is_not_rebased(tmp_path, monkeypatch):
    """pytorch/vision#796 opened 2019, last touched 2021: merging main into it
    conflicts and yields an unreviewable diff. Start fresh, credit the author."""
    import subprocess as sp
    from oss_pipeline import repo
    from oss_pipeline.models import PRSignal

    r = tmp_path / "r"; r.mkdir()
    sp.run(["git", "init", "-q", "-b", "main", str(r)], check=True)
    (r / "f.txt").write_text("x")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "init"], check=True)
    sp.run(["git", "-C", str(r), "branch", "-f", "origin/main", "main"], check=True)

    monkeypatch.setattr(repo, "default_branch", lambda c: "main")
    monkeypatch.setattr(repo, "_git", lambda c, *a, **k: "")
    monkeypatch.setattr(repo, "pipeline_env", dict)
    calls = []
    monkeypatch.setattr(repo.subprocess, "run",
                        lambda *a, **k: calls.append(a) or sp.CompletedProcess(a, 0, "", ""))

    c = cand(repo="pytorch/vision", issue=598)
    c.pr_signal = PRSignal(number=796, url="u", author="Miladiouss", is_draft=False,
                           days_since_commit=1800, days_since_author_comment=None,
                           days_since_changes_requested=None, has_stale_label=False,
                           checks_failing=False)
    repo.takeover_branch(c, r)
    # It must never have fetched the ancient PR head.
    assert not any("pull/796/head" in str(a) for a in calls), calls


def test_verify_prompt_warns_against_inferring_blanket_refusal():
    """Regression: the verifier read 'Unfortunately not; mainly because
    np.histogram...' as refusing the whole feature and blocked a good patch."""
    from oss_pipeline import implement
    p = implement.VERIFY_PROMPT.lower()
    assert "np.histogram" in p and "not refusing the feature" in p
    assert "never infer" in p


def test_brief_prompt_requires_self_contained_prohibitions():
    from oss_pipeline import brief
    assert "self-contained prohibition" in brief.PROMPT


@pytest.mark.parametrize("rc,out", [
    (4, "ImportError while loading conftest ... ModuleNotFoundError: No module named 'numpy'"),
    (1, "RuntimeError: operator torchvision::nms does not exist"),
    (127, "/bin/sh: go: command not found"),
    (5, "no tests ran"),
    (1, "ERROR: Could not build wheels for torchvision"),
])
def test_environment_failures_are_not_patch_failures(rc, out):
    """Regression: pytorch/vision cannot be built locally, and blaming the diff
    for that discards correct work."""
    from oss_pipeline import toolchain
    assert toolchain.is_environment_failure(rc, out), (rc, out)


@pytest.mark.parametrize("rc,out", [
    (1, "FAILED test/test_x.py::test_thing - assert 1 == 2"),
    (1, "2 failed, 40 passed in 3.2s"),
])
def test_real_test_failures_are_not_excused(rc, out):
    from oss_pipeline import toolchain
    assert not toolchain.is_environment_failure(rc, out), (rc, out)


def test_reply_prompt_requires_checking_the_diff():
    """Regression: a draft announced the changelog, Fixes keyword and TESTING.md
    as 'not yet done' when the diff already contained all three."""
    from oss_pipeline import replies
    p = replies.DRAFT
    assert "{diff}" in p
    assert "never imply\n  something is outstanding when the diff shows it is done" in p


def test_preflight_inspects_committed_files_too(tmp_path, monkeypatch):
    """Regression: .agents/skills/.../SKILL.md reached a public kornia PR. The
    guard matched the path, but preflight only looked at uncommitted work and
    the file was already committed, so it reported clean."""
    import subprocess as sp
    from oss_pipeline import submit
    r = tmp_path / "r"; r.mkdir()
    sp.run(["git", "init", "-q", "-b", "main", str(r)], check=True)
    (r / "ok.py").write_text("x = 1\n")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "init"], check=True)
    sp.run(["git", "-C", str(r), "branch", "-f", "origin/main", "main"], check=True)
    sp.run(["git", "-C", str(r), "checkout", "-q", "-b", "topic"], check=True)
    (r / ".agents").mkdir()
    (r / ".agents" / "SKILL.md").write_text("agent notes\n")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "sneaks in"], check=True)   # committed, not pending

    monkeypatch.setattr(submit, "pipeline_env", dict)
    monkeypatch.setattr(submit.repo, "default_branch", lambda c: "main")
    # The fetch has no real remote to talk to; it fails quietly, which is the
    # point -- branch_files must still read the local origin/main ref.
    files = submit.branch_files(r)
    assert ".agents/SKILL.md" in files, files
    assert any(submit.AGENT_ARTEFACTS.search(f) for f in files)


def test_deleted_files_are_not_reported_as_shipped(tmp_path, monkeypatch):
    """A file committed earlier in the branch and deleted now is not shipped, so
    it must not keep failing preflight forever."""
    import subprocess as sp
    from oss_pipeline import submit
    r = tmp_path / "r"; r.mkdir()
    sp.run(["git", "init", "-q", "-b", "main", str(r)], check=True)
    (r / "ok.py").write_text("x = 1\n")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "init"], check=True)
    sp.run(["git", "-C", str(r), "branch", "-f", "origin/main", "main"], check=True)
    sp.run(["git", "-C", str(r), "checkout", "-q", "-b", "topic"], check=True)
    (r / ".agents").mkdir(); (r / ".agents" / "SKILL.md").write_text("notes\n")
    sp.run(["git", "-C", str(r), "add", "-A"], check=True)
    sp.run(["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@e",
            "commit", "-q", "-m", "add"], check=True)

    monkeypatch.setattr(submit, "pipeline_env", dict)
    monkeypatch.setattr(submit.repo, "default_branch", lambda c: "main")
    assert ".agents/SKILL.md" in submit.branch_files(r)

    (r / ".agents" / "SKILL.md").unlink()          # removed, not yet committed
    assert ".agents/SKILL.md" not in submit.branch_files(r)


def test_every_live_status_can_reach_a_terminal_outcome():
    """GitHub decides merge and close, not us.

    Any status the watcher can observe a PR in must accept both outcomes.
    Derived rather than enumerated so the hole cannot reopen: this caught
    CHANGES_REQUESTED -> MERGED being missing while a real PR sat in exactly
    that state, where the TransitionError was swallowed and the merge lost.
    """
    from oss_pipeline import policy
    from oss_pipeline.models import TRANSITIONS, Status
    for live in policy.OPEN_STATUSES:
        allowed = TRANSITIONS[live]
        assert Status.MERGED in allowed, f"{live} cannot record a merge"
        assert Status.CLOSED in allowed, f"{live} cannot record a close"


def test_target_repo_commands_see_no_credential_of_any_kind(monkeypatch):
    """A denylist of two names let OPENAI_API_KEY walk straight past.

    `go test` runs code the other project controls. It gets no credential of
    ours, and none of anyone else's that happens to be in this shell.
    """
    from oss_pipeline.identity import sandbox_env

    for var in ("GH_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_API_KEY",
                "AWS_SECRET_ACCESS_KEY", "SOME_SERVICE_TOKEN", "DB_PASSWORD"):
        monkeypatch.setenv(var, f"sentinel-{var}")
    monkeypatch.setenv("PATH", "/usr/bin")

    env = sandbox_env()
    for key, value in env.items():
        assert not value.startswith("sentinel-"), f"{key} leaked to a target repo"
    assert env["PATH"] == "/usr/bin", "ordinary variables must survive"


def _legacy_token_check():
    """`go test` is code the other project controls. It must not get our PAT."""
    import subprocess
    from pathlib import Path
    from unittest import mock
    from oss_pipeline import implement

    seen = {}

    def fake_run(*a, **kw):
        seen.update(kw.get("env") or {})
        return subprocess.CompletedProcess(a[0] if a else "", 0, "", "")

    with mock.patch.object(implement.subprocess, "run", fake_run):
        implement._run(Path("/tmp"), "true")
    assert "GH_TOKEN" not in seen
    assert "GITHUB_TOKEN" not in seen
    assert seen.get("GIT_AUTHOR_EMAIL"), "identity vars should survive"


def test_other_logins_are_not_in_a_tracked_config_file():
    """exclusions.yaml is tracked; identity.env is not. Keep them apart."""
    import yaml
    from oss_pipeline.identity import ROOT
    tracked = yaml.safe_load((ROOT / "config" / "exclusions.yaml").read_text()) or {}
    assert "avoid_repos_touched_by" not in tracked
    # Read the real logins from the gitignored file rather than hardcoding
    # them here -- this test file is tracked too.
    from oss_pipeline.identity import load_identity
    import subprocess
    tracked = subprocess.run(
        ["git", "-C", str(ROOT), "ls-files"],
        capture_output=True, text=True).stdout.split()
    if not tracked:                      # not a git repo yet -- nothing to leak
        return
    for login in load_identity().other_logins:
        for rel in tracked:
            f = ROOT / rel
            try:
                blob = f.read_text()
            except (UnicodeDecodeError, OSError):
                continue
            assert login not in blob, (
                f"{login} appears in tracked file {rel}; it links the accounts")


def test_untracked_agent_file_is_visible_to_preflight(tmp_path):
    """An untracked file commit() would sweep in must be caught first.

    `git diff --name-status HEAD` lists tracked changes only, so a file the
    implementer created and never committed was invisible to branch_files() --
    while commit() runs `git add -A`. Same shape as the .agents incident that
    reached a public PR: the rule was right, the scope was wrong.
    """
    import os
    import subprocess
    from oss_pipeline import submit

    clone = tmp_path / "clone"
    clone.mkdir()
    env = {**os.environ, "GIT_AUTHOR_NAME": "T", "GIT_AUTHOR_EMAIL": "t@example.com",
           "GIT_COMMITTER_NAME": "T", "GIT_COMMITTER_EMAIL": "t@example.com"}
    def git(*args):
        subprocess.run(["git", "-C", str(clone), *args], check=True,
                       capture_output=True, env=env)
    git("init", "-q", "-b", "main")
    (clone / "a.txt").write_text("x\n")
    git("add", "-A")
    git("commit", "-qm", "init")

    agents = clone / ".agents" / "skills"
    agents.mkdir(parents=True)
    (agents / "SKILL.md").write_text("agent workflow notes\n")

    files = submit.branch_files(clone)
    assert ".agents/skills/SKILL.md" in files, (
        f"an untracked file `git add -A` would ship must be visible: {files}")


def test_private_addresses_are_caught_without_being_hardcoded():
    """The work address must be detected, but must not live in this repo.

    Writing the address that scan_secrets exists to suppress into a tracked
    file would publish it the moment this repository became public, so the
    value is read from the gitignored identity configuration instead.
    """
    from oss_pipeline import submit
    from oss_pipeline.identity import load_identity

    work = load_identity().work_email
    assert work, "identity.env should carry WORK_EMAIL"
    assert "a private address or login" in submit.scan_secrets(f"contact {work} for access")
    assert submit.scan_secrets("contact someone@example.com for access") == []

    # And the address itself must appear in no tracked file.
    import subprocess
    from oss_pipeline.identity import ROOT
    tracked = subprocess.run(["git", "-C", str(ROOT), "ls-files"],
                             capture_output=True, text=True).stdout.split()
    for rel in tracked:
        try:
            body = (ROOT / rel).read_text()
        except (UnicodeDecodeError, OSError):
            continue
        assert work not in body, f"{work} appears in tracked file {rel}"


def test_loaders_ignore_fields_written_by_a_newer_writer(tmp_path, monkeypatch):
    """Both loaders, not just the candidate one.

    The Go rewrite writes the same files and has fields this version does not.
    Guarding _hydrate and forgetting load_repo_facts cost a full day of
    discovery: phase B2 calls it for every candidate, the TypeError escaped
    the stage, and the sweep found nothing at all.
    """
    import json
    from oss_pipeline import store
    from oss_pipeline.models import RepoFacts, Status

    facts = {f.name: getattr(RepoFacts(repo="a/b"), f.name)
             for f in dataclasses.fields(RepoFacts)}
    facts["repo"] = "a/b"
    facts["topics"] = ["from-the-future"]          # Go writes this; we do not

    monkeypatch.setattr(store, "REPOS", tmp_path / "repos")
    (tmp_path / "repos").mkdir()
    (tmp_path / "repos" / "a__b.json").write_text(json.dumps(facts))
    assert store.load_repo_facts("a/b").repo == "a/b"

    cand = {"repo": "a/b", "issue": 1, "title": "t", "url": "u",
            "status": Status.PROPOSED.value, "facts": facts,
            "took_over": True}                      # and this
    monkeypatch.setattr(store, "DIR", tmp_path / "candidates")
    (tmp_path / "candidates").mkdir()
    (tmp_path / "candidates" / "a__b__1.json").write_text(json.dumps(cand))
    loaded = store.load("a__b__1")
    assert loaded.status is Status.PROPOSED
    assert loaded.facts.repo == "a/b"


def test_an_unreadable_candidate_is_never_treated_as_unseen(tmp_path, monkeypatch):
    """should_reconsider returning True is how discover decides to overwrite.

    A file it cannot parse is not a candidate we have never seen, and saying
    so destroyed four approvals and twelve rejections in one sweep.
    """
    from oss_pipeline import store

    monkeypatch.setattr(store, "DIR", tmp_path)
    (tmp_path / "broken.json").write_text("{not json at all")
    assert store.should_reconsider("broken", after_days=14) is False
    assert store.should_reconsider("never-seen-at-all", after_days=14) is True
