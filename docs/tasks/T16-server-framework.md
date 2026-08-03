# T16 — Server framework design

**Agent:** — (human-led) · **Milestone:** M5 · **Depends on:** —

**Owns:** `docs/SERVER-DESIGN.md`. No `imapserver` code, at all, under any
circumstances, until the design is approved.

## Status

Design drafted 2026-08-03, **revision 3** the same day after two review rounds:
[`docs/SERVER-DESIGN.md`](../SERVER-DESIGN.md). **Awaiting human approval.**
Approval means approving its §2 (backend abstraction), §6 (reference backend
ships supported), §8 (resource limits as a first-class design item) and §9 (the
v1.0 cost, and `imapserver` as a nested v0.x module).

Review 1 → revision 2: rewrote the concurrency model (revision 1's was internally
inconsistent), added a compilable interface sketch, replaced bare type-assertion
capability derivation with a descriptor table, resolved a self-contradiction over
who owns SEARCH, and **reversed** the versioning recommendation from a
same-module carve-out to a nested module.

Review 4 → revision 5: **revision 4's batch-coalescing rule destroyed origin
information** — a batch carries one `Origin`, so merging two with different
origins produces an untrue origin and mis-suppresses changes even with the
revision chain intact. Batches are never merged; the order is validate → account
→ coalesce wire-level changes only. Plus the updater lifetime contradiction and
the exact-match origin rule. **The reviewer's position after this revision is
approve**, with T17/T18/T21 and the dependent specs clear to proceed — the human
approval is still outstanding and is what this task waits on.

Review 3 → revision 4, which closed the last contract gap: **"updates carry the
revision they follow" was not implementable**, since one backend commit produces
several events and per-event revisions leave the before/after sense, duplicate
tokens and coalescing all undefined. Updates now publish as **batches** with an
explicit `Before → After` chain. Also: capability descriptors split from
**feature** descriptors (the BINARY case proves an options field can be activated
by a revision *or* a capability); `MoveMailbox` required before advertising
`MOVE` or `IMAP4REV2`, since MOVE must not be synthesised from
Copy+Store+Expunge; and five smaller corrections. The rev2/BINARY split flagged
as unverified in revision 3 is **confirmed** and now stated as fact.

Review 2 → revision 3, which closed the one remaining architectural blocker:
**revision 2 promised the framework owns the sequence-number map but gave it no
way to build one atomically.** `Select` now takes the `*Updater` and returns a
snapshot, capturing state and attaching updates in a single backend operation.
Also: SEARCH criteria reach the backend UID-normalised through a type only the
framework can construct; the writer topology is decided (synchronous, on the
event loop); `VANISHED` may be coalesced after all while `EXPUNGE` may not; blanket
echo suppression became per-operation origin accounting; the options/capability
pairing got two mechanical gates.

Full change list in `.state/progress/T16.md`; the compilable interface sketch
lives there too, along with the provenance record for the rev2/BINARY split.

## Why this task moved earlier

It used to say "do not start before v1.0 is tagged". That was changed
deliberately on 2026-08-03, and the reason is not impatience.

Waiting was justified on the grounds that the architecture had already paid for
the server: `package imap` holds the vocabulary and does no I/O, so the server
can be added without touching an existing signature. That is still true, and it
is still the reason the *implementation* waits.

What it does not cover is this: adding types to `package imap` after v1.0 is
additive and always allowed, but **reshaping an existing type is not** — and a
vocabulary exercised in only one direction can contain a type the server can
consume but cannot naturally produce. No client-side review finds that, because
the client is the direction that works. `SERVER-DESIGN.md` §0 shows the semantic
codec exists in exactly one direction for every one of these types, so the
concern is concrete rather than theoretical.

Design before the freeze; implementation after it. The design is what tells us
whether the freeze is safe.

## Design document scope

The five original questions, all answered in `SERVER-DESIGN.md`:

1. **The backend abstraction** (§2) — struct-of-functions vs. small mandatory
   interface plus optional capability interfaces vs. hybrid. Recommends the
   hybrid, with the deciding extension pressure named: nine existing RFCs each
   want a method group, so a growable mandatory interface breaks nine times
   before meeting an RFC nobody has written. Backed by a compilable sketch.
2. **Framework versus backend** (§3) — why IDLE, CONDSTORE, QRESYNC, SEARCH,
   AUTHENTICATE, NOTIFY and NAMESPACE are cooperative rather than delegable, and
   the capability descriptor table that derives advertisement from backend type
   *and* connection state.
3. **Concurrency model** (§4) — reader goroutine, bounded command queue, event
   loop, single writer; sequential command execution; the backend re-entrancy
   contract in writing; the full update contract (UID identity, ordering,
   lifetime, coalescing, echo suppression) and the overflow policy.
4. **The in-memory reference backend** (§6) — ships as supported
   `imapserver/memory`, decided 2026-08-03.
5. **Testing strategy** (§7) — loopback is the inner loop, not validation;
   `imaptest`, the existing interop matrix pointed at our own server, real client
   software, server-side fuzzing and stateful security tests are.

Plus three the original spec did not have:

6. **Protocol baseline** (§1) — rev1 wire, advertise rev2, switch on `ENABLE`;
   per-connection enabled state. Revision 1 used rev1 and rev2 facilities
   interchangeably without ever stating which was supported.
7. **Resource limits** (§8) — the threat model inverts. The server faces hostile
   *unauthenticated* clients, a larger exposure than the client's hostile-server
   case, and the bounds must cover execution cost and response size, not just
   parser inputs.
8. **What this costs v1.0** (§9) — the T17 exit criterion, now backed by three
   confirmed findings rather than a prediction, and the `imapserver` versioning
   question, which needs a written exception to API-STABILITY.

## Done when

- `docs/SERVER-DESIGN.md` is approved by the human, explicitly, in writing.
- The `imapserver` versioning question in §9 is decided: either the recommended
  nested v0.x module is approved and written into `docs/API-STABILITY.md` as a
  real exception, or it is rejected in favour of the same-module fallback — and
  the doc records which, with the reasoning.
- T19, T20 and T22–T25 specs are written against the approved abstraction.

## Do not start

- `imapserver/**` code — before approval, and before v1.0 is tagged. Both.
- T19, T20, T22–T25 specs — before approval. Writing them today would encode a
  guess at the abstraction this task exists to choose.

T17, T18 and T21 are the deliberate exceptions and have their own specs: none of
them depends on the §2 recommendation.
