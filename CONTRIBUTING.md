# Contributing to TIER

Thanks for your interest in improving TIER. TIER measures the yield of AI token
consumption in software development -- outcome per token, not tokens consumed.
Contributions of all sizes are welcome, from typo fixes to new capabilities.

This guide is short on purpose. If anything here is unclear, open an issue and
ask -- questions are contributions too.

## Code of Conduct

This project follows a [Code of Conduct](CODE_OF_CONDUCT.md). By participating,
you agree to uphold it. Please read it before opening your first issue or pull
request.

## Reporting security issues

Do **not** report security vulnerabilities through public issues or pull
requests. Follow the private process in [SECURITY.md](SECURITY.md) instead.

## Start with an issue

Every change should be tied to a GitHub issue.

- **Found a bug or have an idea?** Search the existing issues first. If nothing
  matches, open a new issue that describes the problem, what you expected, and
  how to reproduce it.
- **Want to work on something?** Comment on the issue so others know it is being
  picked up, and to confirm the approach before you invest a lot of time.

Opening an issue before a large change saves everyone effort: it lets us agree
on the design before code is written.

## Development workflow

1. **Fork** the repository and clone your fork.
2. **Branch from `main`** with a short, descriptive name. Use a prefix that
   matches the kind of change:
   - `feature/` -- new functionality
   - `fix/` -- bug fixes
   - `refactor/` -- restructuring without behavior change
   - `chore/` -- dependencies, tooling, maintenance
   - `docs/` -- documentation only

   For example: `fix/negative-usage-tokens`.
3. **Make your change** and add or update tests. Tests are table-driven with
   named cases; a bug fix should include a regression test that fails before
   your fix and passes after it.
4. **Match the surrounding style.** Read the package's existing tests first, and
   keep the project's conventions: money is stored and compared in integer
   micro-dollars (never floats), the tool fails loud rather than falling back
   silently, and new runtime dependencies need a clear, discussed reason.

## Run the checks locally

There is **no hosted CI** -- all checks run on your machine, so please run them
before opening a pull request. Everything must pass under the race detector.

```sh
make lint         # go vet (+ golangci-lint if installed)
make check        # lint + build + race-enabled unit tests
make check-full   # everything in `make check` plus integration tests
```

Fix every failure before you push. A pull request that does not pass
`make check` cannot be merged.

## Commit messages

Use a conventional prefix so history stays readable:

- `feat:` -- new feature
- `fix:` -- bug fix
- `test:` -- adding or fixing tests
- `refactor:` -- code restructuring
- `chore:` -- dependencies, config, maintenance
- `docs:` -- documentation only

Write in the imperative mood and reference the issue, for example:
`fix: reject negative usage tokens in the JSONL collector (#123)`.

### Sign your commits (DCO)

Contributions are accepted under the [Developer Certificate of
Origin](https://developercertificate.org/): a simple statement that you wrote
the change, or otherwise have the right to submit it under the project's
license. Certify it by adding a `Signed-off-by` line to each commit, which
`git` adds for you:

```sh
git commit -s -m "fix: reject negative usage tokens (#123)"
```

The line must match the name and email you commit with.

## Open a pull request

1. Push your branch to your fork and open a pull request against `main`.
2. Describe **what** changed and **why**, and **link the issue** it addresses
   (for example, "Closes #123").
3. Confirm in the description that `make check` and `make check-full` pass
   locally.
4. A maintainer will review your pull request. Accepted changes may be
   incorporated into a subsequent tagged release; your authorship and the
   discussion stay on the pull request.

Please be patient during review, and feel free to push follow-up commits in
response to feedback rather than force-pushing, so the discussion stays easy to
follow.

## Licensing of contributions

TIER is licensed under the **Apache License 2.0** (see [LICENSE](LICENSE)). By
contributing, you agree that your contributions are licensed under the same
terms. You retain copyright to your work; the Apache-2.0 license and your DCO
sign-off are what let the project distribute it.

Thank you for contributing.
