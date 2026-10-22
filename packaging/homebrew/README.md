# Homebrew tap

`tiermetric/tap` is the Homebrew tap for `tierd` on macOS. Install with:

```sh
brew install tiermetric/tap/tierd
```

The formula builds `./cmd/tierd` from the tagged source archive using Go with
CGO disabled. It stamps the version with the release tag, including the leading
`v`, and the commit with the required 40-character source commit SHA. These
stamps match release binaries, including the commit reported by `/api/v1/livez`.

The mirror-only `homebrew-render` release job waits for the image and binary CVE
gates, verifies the tagged source archive's embedded commit equals `GITHUB_SHA`,
hashes the archive, and runs
`scripts/render-homebrew-formula.sh <tag> <commit> <sha256> <out-file>` with
`GITHUB_SHA` as the commit. Render and push run as separate jobs so tag code never
runs beside the key. Behind the protected `release` environment, `homebrew` uses a
GitHub App token scoped only to `tiermetric/homebrew-tap` to update
`Formula/tierd.rb` on `main`. It commits and pushes only when the formula changes;
an unchanged formula produces a notice and a successful no-op.

Operators must provide the environment variable `TAP_APP_CLIENT_ID` and environment
secret `TAP_APP_PRIVATE_KEY`, both in environment `release`; never store the key as
a repository secret. Install the App only on
`tiermetric/homebrew-tap` with Contents write permission, and protect `release`
with a required reviewer and tag-only deployment rules.
