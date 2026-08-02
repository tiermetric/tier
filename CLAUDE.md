<!--
  Guidance for AI coding assistants working in this repository.
  Keep this file focused on how to build, test and reason about TIER itself.
-->

# TIER — Token Impact & Efficiency Ratio

Guidance for working in this repository with Claude Code or any other agent.

## What this is

TIER measures the *yield* of AI token consumption in software development —
outcome per token, not tokens consumed. It is an open-source Go tool.

**Stack (what is actually built today):**

- Go (see `go.mod` for the exact toolchain floor).
- SQLite as the single embedded datastore (single file, single-writer, via
  `modernc.org/sqlite` — pure Go, no CGO).
- Standard-library `net/http`; a single process; **single-tenant**.
- The dashboard is a static HTML page served by Go with vanilla-JS client-side
  rendering (`internal/dashboard`) — there is no SPA framework.
- Token capture is JSONL-first (Claude Code session files) with an optional
  reverse proxy.

There is no event bus and no second datastore. If a document elsewhere
describes multi-tenancy, an alternate datastore, or a message bus, treat it as
design intent, not shipped behavior.

## Build, test, run

All checks run locally. Run them before opening a pull request.

```sh
make build        # build ./cmd/tierd into ./bin
make lint         # go vet (+ golangci-lint if installed)
make check        # lint + build + race-enabled unit tests
make check-full   # everything in `make check` plus integration tests (-tags integration)
```

Run the binary:

```sh
./bin/tierd version
./bin/tierd score --repo .
```

See `README.md` for the quickstart, configuration, and `config.example.yaml`.
Additional docs live in `docs/` (how it works, cost normalization, the
reference price table, and the quality-degradation spec).

## Conventions

- **Zero new runtime dependencies** without a clear, discussed need. The small,
  deliberate dependency set is part of the project's identity.
- **Money is stored and compared in integer micro-dollars**, never floats.
  Cost correctness beats convenience everywhere it is touched.
- **Fail loud over silent fallback.** The tool rejects `$0` price entries and
  refuses a non-loopback bind without an auth token, on principle. Match that
  posture in new code.
- **Tests are table-driven** with named cases; bug fixes get a regression test
  that fails before the fix. Everything must pass under `-race`.
- Match the style of the package you are editing; read its existing tests first.

## Pull requests

1. Branch from the latest `main`. Use a short, descriptive branch name
   (e.g. `fix/negative-usage-tokens`).
2. Make the change; add or update tests.
3. Run `make check` and `make check-full`; both must be green.
4. Open a pull request describing the change and linking any relevant issue.

Commit message prefixes: `feat:`, `fix:`, `test:`, `refactor:`, `chore:`,
`docs:`.
