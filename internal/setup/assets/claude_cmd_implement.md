---
description: Implementation pipeline — coder produces a diff, Claude reads the real diff, verifier checks (fleet)
argument-hint: "<what to build>"
---
<!-- managed by fleet -->
Task: $ARGUMENTS

1. Narrow the scope yourself (read the relevant files). Ask the user if anything is unclear. Say so if `git status` is not clean.
2. `task_start(agent="coder", mode="opencode", prompt=<narrow task + files + style rule>, cwd=<project>)`.
   The coder edits the working tree directly.
3. `task_result(wait_s=600)`; then **read `git diff` yourself** — don't trust the coder's summary.
4. `task_start(agent="verifier", prompt="<test/build command>")` → returns failures only.
5. On failure: fix small things yourself; for bigger ones send the coder one more round with fix instructions (at most 2).
6. Finish with: diff summary + verifier result. `git commit/push` only if the user asks.
