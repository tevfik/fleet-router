---
description: QA loop on the fleet — hunter finds bugs, validator confirms them, tester writes tests, verifier runs them (fleet)
argument-hint: "<package, directory or file to examine>"
---
<!-- managed by fleet -->
Target: $ARGUMENTS

You are the orchestrator. The fleet does the reading and writing; **you verify everything that ends up in the tree**.

1. `fleet_status` — note roles that are down or on fallback. Read the target yourself first (size, what it does).
2. **In one message** start `task_start(agent="hunter", mode="chat")` on each file/area (≤ 600 lines each, include the source in the prompt)
   and `task_start(agent="tester", mode="opencode", cwd=<project>)` for the packages that lack tests.
3. Collect the hunter findings with `task_result(wait_s=300)`. Hand ALL of them to `task_start(agent="validator", mode="opencode")` with
   the file paths — it returns CONFIRMED / REFUTED / UNCLEAR per claim. Expect most hunter findings to be wrong; that is the point of this step.
4. **Check each CONFIRMED finding yourself** (read the lines). Fix real bugs, or send `coder` a narrow task. A finding you cannot confirm is dropped.
5. Review the tests the tester wrote: they must assert specific values and be able to fail. Run
   `task_start(agent="verifier", prompt="go test ./... (or the project's test command)")`.
   Tests that pass but assert nothing, or that encode a bug as expected behaviour, are rewritten or deleted.
6. Report: confirmed bugs (fixed / open) · refuted claims (count) · tests added (files) · verifier result.
   `git commit` only if the user asks.
