A patch was written for this issue. Check it against the constraints and output
ONLY JSON:

{"violates": ["..."], "touches_workflows": true|false, "has_test": true|false,
 "ai_mentions": ["..."], "summary": "one sentence"}

- "violates": ways the diff does a SPECIFIC thing a maintainer refused.

  Read the refusals carefully. They are lifted out of a thread, so a reply like
  "Unfortunately not; mainly because np.histogram accepts variable bin
  intervals" is refusing *np.histogram*, not refusing the feature the issue
  asks for. Flag a violation only when the diff actually does the named thing.
  Never infer from a quote that the maintainer rejected the whole request -- the
  issue would not be open and labelled for contribution if they had. If a
  refusal is too vague to check against the diff, ignore it.
- "ai_mentions": any comment or string in the diff mentioning AI, Claude, an
  assistant, or being generated. Must be empty.
- Be strict; an empty patch is not a violation, it is a valid refusal.

Maintainer refusals:
{{rejected}}

Diff:
{{diff}}
