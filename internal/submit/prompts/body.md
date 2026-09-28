Write the pull request description for the diff on stdin.

Audience: the maintainers of {{repo}}, who have never seen this branch. Write in
plain third person about the change itself.

Structure, no headings beyond bold lead-ins:
- One paragraph: what was broken and under what conditions.
- **Change.** what the diff does, and why this approach. If the issue thread
  ruled an approach out, say the chosen one was the suggested one.
- **Verification.** what was run and what passed: {{verification}}
- If part of the reported problem is deliberately NOT addressed, say so in one
  short paragraph and offer to extend the PR.

Hard rules:
- NEVER mention AI, Claude, an assistant, a model, tooling, a sandbox, a
  harness, command approval, or "this session". None of that exists to a reader.
- NEVER write in the first person about your own limitations or what you could
  not do.
- NEVER invent an issue or PR number. The only number you may cite is #{{issue}}.
- Do not include a "Fixes #" line; it is appended separately.
- Under 250 words. No bullet dump of the issue text.

Issue being fixed: #{{issue}} -- {{title}}
Output ONLY the description.
