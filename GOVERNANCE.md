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
- may **merge their own PR only after a code-owner approval**, with green
  required checks and every review thread resolved (see
  [How changes get merged](#how-changes-get-merged)).

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
maintainers. A PR merges when **all three** hold:

1. all **required checks are green**,
2. **every review thread is resolved**, bots' threads included (the
   `main` ruleset requires it), and
3. it has an **approving review from a code owner** of the files it
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
  (`mage CheckRelease`: `celeris.Version` in `server.go` and its line in
  `api/celeris.txt`, the four `middleware/*/go.mod` pins, the README
  "What's new" heading), runs the full CI, and only then creates the tag
  and the GitHub Release. A stale stamp means no tag is created, so there
  is nothing to undo.
- Before that, the stamps are moved in one normal PR, the **last PR before
  the release**: `VERSION=vX.Y.Z mage PrepRelease` rewrites all of them and
  leaves a placeholder under the README heading that `CheckRelease` refuses
  until the release prose is written. Between releases the four
  `middleware/*/go.mod` pins stay at the last released tag on purpose: a
  sub-module that requires an unreleased root version cannot be consumed at
  a pseudo-version (probatorium pins celeris `main` between releases). CI
  runs `mage CheckRelease` on every PR, so the stamps cannot drift apart or
  be half-moved.
- The workflow runs only after the
  [goceleris/probatorium](https://github.com/goceleris/probatorium)
  **nightly** validation matrix and the **weekend soak** have passed on
  the release candidate. A release that has not been through both is not
  cut.
- Sub-module tags (`middleware/<name>/vX.Y.Z`) are created by the same
  workflow, at the same commit.
- If a GitHub Release is ever created by hand, the workflow runs the same
  version-stamp gate and the same CI, and then **stops**: the sub-module
  tags and the Go-proxy notification are reached only from the dispatch
  path. Recovery is therefore still open: while
  `https://proxy.golang.org/github.com/goceleris/celeris/@v/vX.Y.Z.info`
  answers 404 nobody has fetched the version and
  `gh release delete vX.Y.Z --cleanup-tag` is harmless; once the proxy has
  served it the version is burned and the fix ships as the next patch.
- **The workflow's tag creation needs a ruleset bypass that is not granted
  today.** The `Release tags` ruleset blocks `creation` on `refs/tags/v*`
  and lists only `OrganizationAdmin` as a bypass actor, so
  `gh release create` running as `github-actions[bot]` is refused. Until
  the Actions identity is added to that ruleset's bypass list, the
  dispatch path stops at the tag and the maintainer must create the tag by
  hand — which lands on the stop-early path above. Every release cut so
  far has been hand-tagged, so this path has never executed.
- **Release notes are generated from PR labels** (`breaking`, `security`,
  `bug`, `performance`, `enhancement`; see
  [`.github/release.yml`](.github/release.yml)) plus hand-written
  highlights at the top. Label your PR correctly and it will appear in the
  right section.
- **GitHub Releases is the changelog.** There is no `CHANGELOG.md`, and
  none should be added.
- Security fixes follow [SECURITY.md](SECURITY.md); a fix may ship as a
  patch release outside the normal cadence.

## Compatibility

From v1.6.0, the supported API is every exported identifier of the
packages that have no `internal` element in their import path and are
not marked experimental: `celeris`, `celeristest`, `observe`, the
`middleware/...` packages (the four nested modules `compress`,
`metrics`, `otel` and `protobuf` included), and `driver/postgres`,
`driver/redis` and `driver/memcached`.

- **Supported packages follow the
  [Go 1 compatibility promise](https://go.dev/doc/go1compat)** within a
  major version, with the exceptions that promise lists, among them
  security fixes, bugs, unspecified behaviour, struct literals and
  methods. A minor or patch release may add API, including fields of an
  exported struct (`observe.EngineMetrics` gains counters this way) and
  methods of an exported type, so write struct literals of celeris types
  with field names. It does not remove or change an exported identifier,
  a documented default or documented behaviour in a way that breaks code
  that uses it as documented. A change that would do so needs a new
  major version.
- **An internal type that a supported package names through an alias**
  (for example `redis.Value`, `redis.KV`, `redis.Type`,
  `postgres.PGError`, `postgres.TypeCodec`, the drivers' `PoolStats` and
  `PoolWorkerStats`, and `middleware/jwt`'s `Claims` and `Token`) is
  supported through that alias: its exported fields and methods
  follow the promise above. The one exception is the provider type
  under "Not covered" below.
- **Packages marked experimental** in their package documentation may
  change or go away in a minor release, and the release notes say when
  they do. Today that is `validation`. A supported identifier whose type
  is experimental follows the experimental level: today that is
  `observe.Snapshot`'s `ValidationCounters` field, of type
  `validation.Counters`, which exists only under `-tags=validation`.
- **Not covered:**
  - Every package with an `internal` element in its import path:
    `internal/**`, `driver/internal/**`, `middleware/internal/**` and
    `middleware/jwt/internal/**`. Go lets only packages under the
    directory that contains `internal` import one, so code outside
    `github.com/goceleris/celeris/` cannot; the nested middleware
    modules, which are under that path, can.
  - `cmd/celeris`, the validation launcher, and `test/**`, the
    conformance, spec and benchmark suites, which export nothing.
  - What an exported identifier's documentation says is not supported:
    the type `Server.EventLoopProvider` returns, and the drivers'
    `ServerProvider` method that returns it, until
    [#453](https://github.com/goceleris/celeris/issues/453) defines a
    public engine interface.
  - The environment variables marked unsupported below, and every
    variable only tests and the build read (`CELERIS_REQUIRE_*`, the
    driver test addresses such as `CELERIS_PG_DSN`, and the numbered
    ones such as `CELERIS_589_*`).

The tuning variables the engines read at startup (the README's
[table](README.md#tuning-environment-variables) says what each does):

| Variable | Level |
|---|---|
| `CELERIS_ADAPTIVE_START` | supported |
| `CELERIS_MAX_IOURING_TIER` | supported |
| `CELERIS_IOURING_SEND_ZC` | supported |
| `CELERIS_IOURING_MULTISHOT_RECV` | experimental |
| `CELERIS_IOURING_PBUF_COUNT` | experimental |
| `CELERIS_IOURING_FIXED_FILES` | unsupported |
| `CELERIS_ADAPTIVE_DEBUG` | unsupported |
| `CELERIS_DEBUG_*` | unsupported |

A supported variable keeps its name, its documented values and their
effect. An experimental one may change or go away in a minor release, with
a release note. An unsupported one is for development and diagnostics, and
may change or go away in any release.

## Changing this document

Governance changes go through a PR like any other change, reviewed by a
maintainer. Once there is more than one maintainer they require lazy
consensus as described above.
