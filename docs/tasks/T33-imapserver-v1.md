# T33 — `imapserver` v1: full API review, release window and tag

**Issue:** [#54](https://github.com/kiliant/go-imap/issues/54)

**Milestone:** M7, `imapserver` v1 (`../ROADMAP.md`).
**Depends on:** completed T32, and then the release window below.

## Why this task exists

`imapserver` stays on v0.x by an approved exception (`../API-STABILITY.md`),
so that its backend abstraction is not frozen before it has been tested from
outside. On 2026-10-15 the human approved three exit criteria for that
exception. All three must hold:

1. **A second backend.** The T32 Maildir backend, written against the
   exported API only.
2. **Two quiet releases.** Two consecutive `imapserver` minor releases, the
   first at or after the one that contains T32, in which `apidiff` reports no
   incompatible change and `CHANGELOG.md` has no BREAKING entry. Behaviour
   breaks count as breaks. A release cut only to advance the count does not
   count.
3. **A full review.** An `api-guardian` review of the whole `imapserver`
   surface, with its findings resolved.

This task runs criterion 3, watches criterion 2, and turns the result into a
v1 release.

## Sequence

1. **Full review, as soon as T32 is merged.** It runs before the release
   window opens, so that any breaking fix it requires lands first and does
   not restart the count later.
   - It covers every exported symbol of `imapserver`, `imapserver/backendtest`,
     `imapserver/memory` and `imapserver/maildir`, and checks that the examples
     compile.
   - It applies every rule in `../API-STABILITY.md`, plus its
     `imapserver`-specific gates: the golden interface method-set test, the
     feature-binding gate and capability witnesses.
   - Documented behaviour freezes at v1 just as signatures do, so the review
     also covers documented defaults: `Options`, `Limits`, and backend-facing
     guarantees.
   - The verdict goes on the issue. Fixes are made in their own PRs, through
     the owner of the affected file, named on the issue.
2. **Release window.** Track criterion 2 on the issue release by release,
   with the `apidiff` report and the changelog check for each. Any break
   restarts the window.
3. **Final check, right before the tag.** Re-review the diff since step 1.
   It must report no breaking change and no new exported interface that
   bypasses the optional-interface rule.
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
  one version number for users to think about.

Record the decision and its reason in `../RELEASING.md`.

## Ownership and scope

From completed tasks, narrowly:

- T25: `imapserver` doc comments, for review findings.
- T15: this task's `CHANGELOG.md` entries.

Owned documentation:

- the `imapserver` exception in `../API-STABILITY.md`: close it and state
  that the server module is now under the v1 promise;
- `../RELEASING.md`: the module table, the version-number decision, and the
  root-first ordering, which remains;
- the "Two modules" table and wording in `CLAUDE.md`;
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
