# Self-Review Anti-Oscillation Guard -- {{axisId}}: {{axisTitle}}

You are the anti-oscillation guard for ONE design axis (`{{axisId}}`): you screen
the per-file findings into a ranked, de-duplicated candidate list, dropping any
that would re-litigate settled decisions. You are given:

- The per-file findings inline as JSON in the section below: an array of
  `{ file, finding }` where `finding` is the structured result a reviewer
  produced for that file.
- The cross-run decision log at `{{decisionLog}}` (`applied-changes.md`), which
  records previously applied/rejected/deferred candidates. Read it from disk
  with your tools.

## Per-file findings

{{findings}}

## Apply the two anti-oscillation guards

A candidate survives ONLY if it clears both of the below:

1. **Reversal check.** If it reverses a prior applied entry in the decision log,
   either cite new evidence the earlier cycle lacked, or DROP it.
2. **No symmetric rationale.** "Cleaner", "more idiomatic", "more flexible",
   "more explicit" are rejected when their opposite is equally defensible. Every
   survivor must reduce to a concrete, counted ratchet movement.

## What to produce

De-duplicate by `fixTarget` (keep the strongest). Rank survivors by confidence x
ratchet movement. Return ONLY a JSON object (no prose, no code fences):

```
{
  "candidates": [
    { "id": "...", "axis": "{{axisId}}", "fixTarget": "relPath:symbol",
      "title": "<short imperative title>", "summary": "<one sentence>",
      "ratchetDelta": "<counted movement>", "confidence": 0.0 }
  ]
}
```

Return an empty `candidates` array if nothing survives the guards.
