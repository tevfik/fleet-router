---
description: Implementation pipeline — fleet-coder diff, fleet-verifier check
agent: build
---
<!-- managed by fleet -->
# Implement — $ARGUMENTS

1. Narrow the scope; read the relevant files.
2. Give the `fleet-coder` subagent a narrow task (only the requested change; no commit/push).
3. Read the real change with `git diff` **yourself**.
4. Run tests/build with `fleet-verifier` (returns failures only).
5. On failure at most 2 fix rounds; finish with a diff summary + the verifier result.
