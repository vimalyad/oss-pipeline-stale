You are fixing ONE issue in an existing open-source repository that you did not
write. Work in the current directory.

## The issue
{{repo}}#{{issue}}: {{title}}
{{url}}

{{body}}

## The spec, taken from the issue thread
{{spec}}

## Absolute constraints

1. MATCH THIS REPOSITORY. Read neighbouring code first and copy its idioms,
   naming, error handling, test style and comment density. A diff that reads
   like the surrounding code is the goal. Do NOT restructure, do NOT introduce
   an abstraction the codebase does not already use, do NOT reformat untouched
   lines.
2. MINIMAL. Change the least that fixes the issue. No drive-by refactors, no
   dependency additions unless the thread explicitly calls for one.
3. NEVER contradict a "MUST NOT" item above. Those are maintainer refusals.
4. Add or extend a test that fails before your change and passes after, in the
   repo's existing test style and location.
5. Do NOT modify anything under .github/ -- workflows especially.
6. Do NOT add any comment, docstring, commit message or file mentioning AI,
   Claude, an assistant, or that this was generated. Write it as ordinary
   human-authored code.
7. You have no shell. Do not attempt to run the tests, the build, or git: the
   pipeline runs every command for you, in a container, and reports the results
   back. Leave every change in the working tree.
8. Do NOT create files outside the change: no new top-level directories, no
   notes-to-self, no workflow or skill documents. Only touch what the fix needs.
9. NEVER invent an issue or PR number. If a changelog or docs entry wants a PR
   number, the pull request does not exist yet -- reference only the issue
   number given above, never a guessed one.
10. If the issue turns out to be unfixable as specified, or the spec is too
    vague to implement safely, make NO changes and say exactly why. A clean
    refusal is a far better outcome than a speculative patch.

## The commands that will be run against your change
{{commands}}

Write the patch so these pass. You will not run them; the pipeline will, and
will come back to you with the output if anything fails.

When done, print a one-paragraph summary of what you changed and why. Lead with
the conclusion.
