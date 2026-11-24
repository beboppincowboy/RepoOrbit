---
module: Backend code generation
date: 2026-11-17
problem_type: build_error
component: tooling
symptoms:
  - gqlgen exited with status 1 and no diagnostic under the local Go 1.27 toolchain
  - A failed generation left GraphQL generated files missing
root_cause: config_error
resolution_type: workflow_improvement
severity: medium
tags:
  - gqlgen
  - sqlc
  - go-toolchain
---

# Generate backend code with the pinned Go toolchain

The U5 generation run under Go 1.27 exited without a diagnostic and left generated Go files missing. The subsequent build could no longer resolve generated GraphQL types.

Run `make generate` from the repository root. The Makefile exports `GOTOOLCHAIN=go1.24.6` for both sqlc and gqlgen. For a GraphQL-only regeneration from the backend directory, use:

```sh
GOTOOLCHAIN=go1.24.6 go tool gqlgen generate
```

Let generation finish before building or running Go tests in the same checkout. Use `make check` on the completed changes to check generation drift alongside the normal build.
