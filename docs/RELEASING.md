# Releasing

1. Merge to `main` and wait for CI to be green (`make test lint itest deploy-itest` locally as well).
2. Check what will ship: `make release-snapshot`, then look in `dist/` (archives, `checksums.txt`,
   `homebrew/imapvault.rb`) and run the archive's binary with `--version`.
3. Tag from that commit and push the tag: `git tag v0.1.0 && git push origin v0.1.0`.
4. `release.yml` runs goreleaser and creates a **draft** GitHub release with the archives and `checksums.txt`,
   and commits the Homebrew formula to [michele/homebrew-imapvault](https://github.com/michele/homebrew-imapvault).
   `docker.yml` publishes the container image to `ghcr.io/michele/imapvault`.
5. Read the draft, then publish it with **"pre-release" unchecked** and "latest release" checked
   (`gh release edit vX.Y.Z --draft=false --latest`). `install.sh` finds the newest version through
   `/releases/latest`, which ignores drafts and pre-releases.
6. Publish promptly: until the draft is published, `brew install` and `install.sh` fail because the
   archives answer 404.

## Format version

A release freezes the on-disk format. After v0.1.0, any change to `repo.toml`, the object envelope or the
index schema bumps `format_version` and ships a migration (or a clear refusal). Say so in the release notes.

## One-time setup

- Create the empty repository `michele/homebrew-imapvault` on GitHub (the tap).
- Create the tap token. The workflow's `GITHUB_TOKEN` can only write to this repository.
  1. GitHub > Settings > Developer settings > Personal access tokens > **Fine-grained tokens** > Generate new token.
  2. Resource owner: you. Repository access: **Only select repositories**, `michele/homebrew-imapvault`.
  3. Permissions, Repository: **Contents: Read and write** (Metadata: Read is added automatically). Nothing else.
  4. Set an expiry and note the date: when it lapses, the release still succeeds up to the formula step, then fails.
  5. Add it to this repository as an Actions secret named `HOMEBREW_TAP_GITHUB_TOKEN`
     (`gh secret set HOMEBREW_TAP_GITHUB_TOKEN`).
- Site (Cloudflare Pages, domain `imapvault.sh`): create a Pages project from this repository with production
  branch `main`, **no build command** and output directory `site`, then add `imapvault.sh` as its custom domain.
  `site/install.sh` is the only copy of the installer, so `https://imapvault.sh/install.sh` serves it; check that
  the response is not an HTML page and that `curl -fsSL https://imapvault.sh/install.sh | head -1` prints `#!/bin/sh`.
- The first image push creates the ghcr package as private; make it public in the package settings.

## Notes

- `brews` in `.goreleaser.yaml` is deprecated by goreleaser (casks replace it for macOS apps), so
  `goreleaser check` exits non-zero. It still works; Homebrew casks do not cover Linux, so the formula stays.
- `make release-snapshot` writes the formula to `dist/homebrew/imapvault.rb` without publishing it.
