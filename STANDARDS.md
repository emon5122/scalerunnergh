# Standards

This repo follows Emon's engineering standards: https://github.com/emon5122/titli/blob/main/docs/standards.md
(locked 2026-11-20). Anything here may only ADD to them.

**Stack:** Go (CLI/service: Cobra where it has commands), Docker + native (Windows/Linux) backends.

Done = the real thing works for its real user, not "tests pass". One manually dispatched CI run per PR that runs the real tests; a skipped lane is not green. Existing code moves to the standard when touched.
