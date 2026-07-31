# T16 — Server framework (design first)

**Agent:** TBD · **Milestone:** M5 · **Depends on:** v1.0 tagged

**Owns:** `docs/SERVER-DESIGN.md` initially. No code until the design is approved.

## Why this is after v1.0

Deliberate. The client reaching a stable v1.0 is the project's goal; a server
framework doubles the API surface and would delay it substantially.

Waiting costs nothing **because the architecture already paid for it**:
`package imap` holds the shared vocabulary (envelope, body structure, flags,
search criteria, fetch items) and performs no I/O, so the server can be added as
a new package without touching a single existing signature. That property is
worth protecting — if a client-side change would force server-relevant types into
`imapclient`, reject it.

## Design document scope

Write `docs/SERVER-DESIGN.md` before any code, covering:

1. **The backend abstraction.** The hard problem. A backend interface is a
   permanent compatibility commitment, and every new extension wants to add a
   method — which is precisely the breaking change `docs/API-STABILITY.md` §4
   exists to prevent. Options to evaluate explicitly:
   - struct-of-functions (consistent with the rest of this library)
   - small mandatory interface + optional capability interfaces discovered by
     type assertion (the shape Go's stdlib uses for optional behaviour)
   - a hybrid
   Recommend one, with the extension pressure that decides it named concretely.
2. **Which capabilities the framework implements** versus delegates to the
   backend. `IDLE`, `CONDSTORE` and `QRESYNC` are the interesting cases — they
   need backend cooperation that a naive interface cannot express.
3. **Concurrency model.** Per-connection goroutines, backend re-entrancy
   requirements, and how unsolicited updates are delivered.
4. **An in-memory reference backend** for testing, and whether it ships as a
   supported package or stays internal to tests.
5. **Testing strategy.** The client and server can test each other, but that is
   circular — external conformance testing against Dovecot's and Stalwart's own
   test suites is what would actually validate it.

## Do not start

- before v1.0 of the client is tagged
- without human approval of the design document

Implementation tasks get written after the design is approved, not now — writing
them today would guess at an abstraction that the design phase exists to choose.
