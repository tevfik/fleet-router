---
description: QA loop — fleet-hunter finds bugs, fleet-validator confirms them, fleet-tester writes tests, fleet-verifier runs them
agent: build
---
<!-- managed by fleet -->
# QA — $ARGUMENTS

1. `fleet-hunter` on each file/area (≤ 600 lines per call): "report concrete defects with path:line and the trigger".
   In parallel, `fleet-tester` for packages without tests.
2. Give **all** hunter findings to `fleet-validator` (CONFIRMED / REFUTED / UNCLEAR per claim). Most hunter findings are wrong — expect it.
3. Read the lines of every CONFIRMED finding yourself before touching the code; drop what you cannot confirm.
4. Review the tester's files: assertions must name specific values and be able to fail. Run the project's tests with `fleet-verifier`.
5. Report: confirmed bugs (fixed / open) · refuted claims (count) · tests added · verifier result. Commit only if asked.
