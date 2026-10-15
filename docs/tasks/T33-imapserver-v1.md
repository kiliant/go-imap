# T33 — `imapserver` v1: full API review, release window and tag

**Issue:** [#54](https://github.com/kiliant/go-imap/issues/54)

**Milestone:** M7, `imapserver` v1 (`../ROADMAP.md`).
**Depends on:** completed T32, its contract-gap triage, and then the
release window below.

## Why this task exists

`imapserver` stays on v0.x by an approved exception (`../API-STABILITY.md`),
so that its backend abstraction is not frozen before it has been tested with
persistent storage through its public API. On 2026-10-15 the human approved
three exit criteria for that exception. All three must hold:

1. **A second backend.** T32's persistent Maildir backend, built only from
   the exported API and its documentation, passes `backendtest` and server
   interop.
2. **Two quiet releases.** Two consecutive `imapserver` minor releases, the
   first at or after the one containing T32 and the initial full review's
   fixes. Every release from the first through the second minor, patches
   included, has no incompatible change in `apidiff` and no BREAKING entry
   in `CHANGELOG.md`, including behaviour changes. Each counted minor must
   contain a server-module change beyond a root dependency bump; a release
   cut only to advance the count does not count. Any break restarts the
   window.
3. **A full review.** An `api-guardian` review of the whole server surface,
   with its findings resolved: T33.

This task runs criterion 3, watches criterion 2, and turns the result into a
v1 release.

## Sequence

1. **Full review, once T32 is merged and every contract gap on #53 is
   resolved or explicitly deferred.** A gap blocking T32's required contract
   must be fixed. Review runs before the release window opens, so that any
   breaking fix lands first.
   - It covers every exported symbol of `imapserver`, `imapserver/backendtest`,
     `imapserver/memory`, `imapserver/message` and `imapserver/maildir`, and
     checks that the examples compile. The helper is reviewed with two
     consumers, including its streaming, cancellation and source-lifetime
     guarantees.
   - It applies every rule in `../API-STABILITY.md`, plus its
     `imapserver`-specific gates: the golden interface method-set test, the
     feature-binding gate and capability witnesses.
   - Documented behaviour freezes at v1 just as signatures do, so the review
     also covers documented defaults: `Options`, `Limits`, and backend-facing
     guarantees.
   - Confirm that optional interfaces tested only by memory can acquire a
     successor the framework prefers without breaking old implementations
     or withdrawing advertised capabilities. The base interfaces and shared
     types must have evidence from both backends.
   - Define `backendtest` compatibility at v1: new checks of an existing,
     documented obligation may expose backend bugs; new obligations must
     be opt-in through an extensible harness control or a witnessed
     capability. A suite upgrade must not silently impose new requirements
     on a previously conforming backend. Document this in its package docs.
   - The verdict goes on the issue. Fixes are made in their own PRs, through
     the owner of the affected file, named on the issue.
2. **Release window.** Track criterion 2 on the issue release by release,
   with the `apidiff` report and the changelog check for each. Any break
   restarts the window.
3. **Final check, right before the tag.** Re-review the diff since step 1.
   It must report no breaking change and no new exported interface that
   bypasses the optional-interface rule. Run `apidiff` from the second quiet
   release to the candidate tag: either the exported surface is unchanged,
   or every addition has review and validation before it freezes. A breaking
   result restarts the release window; it cannot be waved through at tagging.
4. **Release.** Follow `../RELEASING.md`, with the root module first if it
   is released at the same time.

## The version number

Go accepts any `v1.x.y` as a module's first v1 tag, and the module path does
not change below v2. So decide here between:

- **`imapserver/v1.0.0`.** The two lines stay independent, and only the
  promise is shared.
- **The root module's next version number.** From then on, both modules are
  tagged together at every release, root first. A module that did not change
  still gets the tag. The cost is releases with nothing in them; the gain is
  one version number for users to think about. Lockstep applies only within
  a shared major: a forced major bump would change import paths for users
  of an otherwise compatible module. Decide and document an escape rule
  when one module needs v2 before the other. This cost favours independent
  version lines unless there is a concrete reason to align them.

Record the decision and its reason in `../RELEASING.md`.

## Ownership and scope

From completed tasks, narrowly:

- T25: `imapserver` doc comments, for review findings.
- T20: `imapserver/backendtest` package documentation for the v1 suite
  compatibility policy; no suite-logic change is handed over.
- T15: this task's `CHANGELOG.md` entries.

Owned documentation:

- the `imapserver` exception in `../API-STABILITY.md`: close it and state
  that the server module is now under the v1 promise;
- `../RELEASING.md`: the module table, the version-number decision, and the
  root-first ordering, which remains;
- the "Two modules" table and wording in `CLAUDE.md`, limited to the factual
  transition to the approved v1 promise in the release PR;
- M7's status in `../ROADMAP.md`.

`.github/scripts/apidiff.sh` already chooses enforcing mode per module from
the baseline tag's major version. After the first `imapserver` v1 tag, an
incompatible server change fails CI with no workflow edit. Confirm this on
the first PR after the tag.

## Done when

- All three exit criteria hold, with the evidence for each on the issue.
- The `imapserver` v1 tag is published and signed, and the release gates in
  `../RELEASING.md` pass. That includes the standalone build and an import
  from a clean module outside the repository.
- `apidiff` runs in enforcing mode for the server module on the first PR
  after the tag.
- `../API-STABILITY.md`, `../RELEASING.md`, `CLAUDE.md`, `../ROADMAP.md` and
  `CHANGELOG.md` describe the new state.
