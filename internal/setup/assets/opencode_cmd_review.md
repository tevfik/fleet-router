---
description: Diff/PR review — fleet-scout context, 4 parallel fleet-architect axes, one verdict
agent: build
---
<!-- managed by fleet -->
# Code review — $ARGUMENTS

1. `fleet-scout` subagent: "list file:line relevant to the change under review, at most 30 files".
2. **Start 4 `fleet-architect` at once** (`background: true`), each with the scout output + its relevant diff: security/data · performance · test coverage · API compatibility. Each axis does only its own job.
3. If an axis says "this may not work", run only that test with `fleet-verifier`.
4. Merge: `Verdict: APPROVE | CHANGES-REQUESTED | BLOCK` + blocking / suggestions / missing tests (path:line). If nothing blocks, say a plain APPROVE.
