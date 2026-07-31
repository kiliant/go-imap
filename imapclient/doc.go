// Package imapclient implements an IMAP4rev1 and IMAP4rev2 client.
//
// The client speaks the RFC 3501 (IMAP4rev1) wire protocol as its baseline and
// activates RFC 9051 (IMAP4rev2) behaviour when the server advertises and
// enables it. The public API presents the IMAP4rev2 shape in both cases,
// emulating it where a rev1 server lacks the native command; such emulation is
// documented on the individual method, including where it is not atomic.
//
// # Concurrency
//
// Commands are pipelined. A command method may synchronously write a bounded
// command prelude before returning its handle; it does not wait for the server
// response. Its Wait method blocks for completion. A single reader goroutine
// demultiplexes tagged completions from unsolicited untagged data. A
// command-specific collector gets first refusal of an untagged response;
// responses no collector claims are delivered to the connection's
// UnilateralDataHandler.
//
// # Cancellation
//
// IMAP has no general command-abort. Cancelling the context of a command that is
// already on the wire therefore invalidates the connection: the client closes it
// rather than leaving the stream desynchronised. IDLE cancels cleanly with
// DONE after the server has accepted it; cancellation before its continuation
// follows the general rule because DONE is not yet valid.
//
// # Status
//
// The connection/session layer, authentication, base mailbox and message
// commands, capability negotiation, ENABLE, and IDLE are implemented and
// verified across their native server matrices. Extension support is still
// under active development; see docs/ROADMAP.md.
package imapclient
