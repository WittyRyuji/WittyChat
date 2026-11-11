# Releasing / 发布流程

Releases use annotated tags such as `v0.1.0-alpha`. Prereleases must be marked as
such in GitHub; a local development tag is not itself a published release.

1. Update `VERSION`, `CHANGELOG.md` and versioned release notes. Record limitations
   honestly; review license notices and scan tracked files/history for secrets.
2. Build both frontends, run Go tests with a dedicated PostgreSQL test database
   (`go test -p 1 ./...`), vet/lint, frontend tests and relevant integration checks.
3. Merge reviewed changes to `main` without rewriting existing history.
4. Create an annotated version tag on that exact commit and push it.
5. The release workflow reruns CI, builds a Windows x64 portable archive, collects
   notices/build metadata and creates checksums before publishing a GitHub release.
   GitHub's short-lived `GITHUB_TOKEN` supplies release permission; SSH alone only
   authorizes Git operations, not Release API requests.
6. Verify the workflow conclusion, tag/commit, prerelease flag, uploaded asset
   names and SHA-256 checksums. Do not claim completion until GitHub confirms it.

`scripts/package-release.ps1` can reproduce the Windows archive locally after
dependency installation and frontend builds. Code signing is not configured.
The package deliberately excludes credentials, runtime databases, object files,
debugging profiles and configuration copied from the maintainer's machine.

Never move an existing public tag or overwrite released assets silently.
If verification fails, fix the workflow and disclose the failure; publish a new
version rather than rewriting an already published release.

Before a release has been published, the workflow may be retried with
`workflow_dispatch` selecting its existing tag, or by pushing a new
`release/v0.1.0-alpha` retry branch containing workflow-only fixes.
The jobs check out and verify the original immutable tag, not the retry branch.
Do not use the retry path to replace an already published asset.
