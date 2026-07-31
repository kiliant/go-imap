// Package imapwire implements the IMAP wire grammar: lexing, decoding and
// encoding of the productions defined in RFC 3501 section 9 and RFC 9051
// section 9.
//
// This package is internal and must never become reachable from an exported
// signature of the module, so that it can be rewritten without a breaking
// change. See docs/API-STABILITY.md section 6.
package imapwire
