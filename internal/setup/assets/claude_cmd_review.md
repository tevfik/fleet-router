---
description: Diff/PR review — scout narrows the context, 4 parallel architect axes, Claude verifies (fleet)
argument-hint: "[scope: file, branch, 'staged'…]"
---
<!-- managed by fleet -->
Review target: $ARGUMENTS (default: `git diff HEAD`)

You are the orchestrator: delegate the heavy reading/analysis to the fleet and merge the result yourself.

1. `fleet_status` — note any role that is down or on fallback.
2. List the changed files yourself. If the diff is over 500 lines, run `task_start(agent="scout", mode="opencode")`
   asking for "relevant file:line list (max 30)" and collect it with `task_result(wait_s=120)`.
3. **In a single message, start 4 × `task_start(agent="architect", mode="chat")`**: security/data · performance · test coverage · API compatibility.
   Give each only the scout output + its relevant diff part (5–15k tokens, not the whole repo). Output format: `Finding: path:line — issue → suggestion`.
4. Collect with four `task_result(wait_s=300)` calls. A failed/timed-out axis → "SKIPPED: reason".
5. **Verify the findings yourself** (read the file); drop whatever you cannot confirm.
6. Output: `Verdict: APPROVE | CHANGES-REQUESTED | BLOCK` + blocking / suggestions / missing tests (with path:line).
