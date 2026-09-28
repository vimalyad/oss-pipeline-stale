Write the git commit message for the diff on stdin.

Rules:
- Match the commit style already used in this repository: {{style}}
- Subject line under 72 characters, imperative mood.
- Body: what changed and why, wrapped at 72 columns. Reference the issue as
  "{{issue_ref}}" on its own line.
- Do NOT mention AI, Claude, assistants, being generated, or any tooling used
  to write it. Write as the human author.
- Describe what the change DOES, not what the touched text is about. A commit
  that deletes a long document is "remove X", not a summary of X's subject --
  deleting a 92-line note about MPS work is not an MPS change.
- Output ONLY the commit message. No fences, no preamble.
