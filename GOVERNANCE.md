# Governance

This document describes how the celeris project is run: who holds which
role, how a change gets merged, how decisions are made, and how releases
are cut. It is deliberately short; when it and reality disagree, fix the
document.

## Roles

### Contributor

Anyone who opens a pull request from a fork. No special access is needed.
Contributors get the same CI, the same review, and the same merge rule as
everybody else.

### Member of the `contributors` team

Members of the GitHub org team **`contributors`** have *write* access to
`goceleris/celeris` only (not to the other org repositories). A member:

- can push branches to this repository and open PRs from them;
- can **approve** pull requests;
- may **merge their own PR only after a code-owner approval** and green
  required checks (see [How changes get merged](#how-changes-get-merged)).

Criteria for an invitation: about **three merged, non-trivial pull
requests** and sustained engagement (reviews, issue triage, follow-through
on feedback). A maintainer sends the invitation; there is no application
form — ask in an issue or a PR thread if you think you qualify.

### Maintainer

Maintainers have *admin* access. They cut releases, own
[`.github/CODEOWNERS`](.github/CODEOWNERS), manage the `contributors` team,
and are the final reviewers for the areas they own. The current maintainer
is **@FumingPower3925** (see [MAINTAINERS.md](MAINTAINERS.md)).

## How changes get merged

Every change to `main` goes through a pull request, including changes by
maintainers. A PR merges when **both** hold:

1. all **required checks are green**, and
2. it has an **approving review from a code owner** of the files it
   touches (CODEOWNERS is the source of truth).

Who presses the button:

- the **author**, if they have write access (a `contributors` member or a
  maintainer), or
- the **approving maintainer**, typically by enabling auto-merge so the PR
  lands as soon as checks pass.

**Nobody merges their own PR without a code-owner approval.** The
maintainer's admin **bypass** of branch protection is reserved for
release and infrastructure emergencies (a broken release workflow, a
stuck required check, a security fix that must land before CI recovers).
Every bypass is visible in the repository audit log and should be
followed by a normal PR that explains it.

## Decisions

Today there is a **single maintainer**, so day-to-day decisions are theirs,
made in the open on issues and PRs. Once there is **more than one
maintainer**, decisions move to **lazy consensus**: a proposal (issue or
PR) that receives no objection from a maintainer within **72 hours** is
accepted. An objection blocks until resolved by discussion; if maintainers
cannot agree, the proposal is dropped rather than forced.

Large or breaking changes should start as an issue before code is written
so the design can be discussed without a diff attached.

## Releases

- A release is **cut by the Release workflow, never by hand**. The
  maintainer runs it from `main` with the version as input
  (`gh workflow run release.yml -f version=vX.Y.Z`, or the Actions UI).
  The workflow checks that every version stamp already says `X.Y.Z`
  (`mage CheckRelease`: `celeris.Version` in `server.go`, the four
  `middleware/*/go.mod` pins, the README "What's new" heading), runs the
  full CI, and only then creates the tag and the GitHub Release. A stale
  stamp means no tag is created, so there is nothing to undo.
- Before that, the stamps are moved in one normal PR:
  `VERSION=vX.Y.Z mage PrepRelease` rewrites all of them and leaves a
  placeholder under the README heading that `CheckRelease` refuses until
  the release prose is written. CI runs `mage CheckRelease` on every PR,
  so the stamps cannot drift apart between releases.
- The workflow runs only after the
  [goceleris/probatorium](https://github.com/goceleris/probatorium)
  **nightly** validation matrix and the **weekend soak** have passed on
  the release candidate. A release that has not been through both is not
  cut.
- Sub-module tags (`middleware/<name>/vX.Y.Z`) are created by the same
  workflow, at the same commit.
- If a GitHub Release is ever created by hand, the workflow still runs the
  same checks first and stops before tagging sub-modules or notifying the
  Go proxy. Recovery: while `https://proxy.golang.org/github.com/goceleris/celeris/@v/vX.Y.Z.info`
  still answers 404 nobody has fetched the version and
  `gh release delete vX.Y.Z --cleanup-tag` is harmless; once the proxy
  has served it the version is burned, and the fix ships as the next
  patch version.
- **Release notes are generated from PR labels** (`breaking`, `security`,
  `bug`, `performance`, `enhancement`; see
  [`.github/release.yml`](.github/release.yml)) plus hand-written
  highlights at the top. Label your PR correctly and it will appear in the
  right section.
- **GitHub Releases is the changelog.** There is no `CHANGELOG.md`, and
  none should be added.
- Security fixes follow [SECURITY.md](SECURITY.md); a fix may ship as a
  patch release outside the normal cadence.

## Changing this document

Governance changes go through a PR like any other change, reviewed by a
maintainer. Once there is more than one maintainer they require lazy
consensus as described above.
