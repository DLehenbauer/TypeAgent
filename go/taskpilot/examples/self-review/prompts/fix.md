# Self-Review Fix -- {{axis}}: {{title}}

You are applying ONE ranked self-review candidate to the taskpilot Go codebase.
The candidate is provided as JSON in the section below (with `fixTarget`,
`summary`, `ratchetDelta`, axis, etc.). `{{hasCandidate}}` is `true` when there
is real work to do.

## Candidate

{{candidate}}

## Rules

1. If `{{hasCandidate}}` is not `true`, make NO changes and stop.
2. Re-check the candidate's premise against the CURRENT code first. If the
   `fixTarget` no longer exists, was already addressed, or the premise no longer
   holds, make NO changes and stop -- do not invent alternative work.
3. Otherwise fully realize the stated `ratchetDelta`. This is a greenfield,
   unshipped project: prefer clean design. Backwards compatibility is a
   non-goal. This includes signatures, type shapes, and on-disk/wire formats.
4. Keep the build and tests green. You MAY rewrite tests to assert the new
   behavior, but do not weaken coverage.
5. When you change a contract, update every in-repo caller, test, and doc in the
   same change -- no deprecated aliases, shims, or back-compat wrappers. Keep
   docs in sync.
6. ASCII only; no BOM.

After editing, briefly confirm in one or two sentences what you changed and which
files you touched. Do not run git -- the workflow commits or reverts your work.
