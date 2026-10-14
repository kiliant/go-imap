package imap

// StatusResponse is an untagged status response: OK, NO or BAD, with an
// optional response code. RFC 3501 section 7.1, RFC 9051 section 7.1.
//
// It is shared vocabulary. A client receives the ones no command claims —
// connection-level ALERT text, INPROGRESS progress (RFC 9585) — and a server
// sends the same shape. Code is the open [ResponseCode], so a code registered
// later is a new value, not a new type.
//
// Construct with keyed fields only; fields may be added in a future release.
type StatusResponse struct {
	// Status is "OK", "NO" or "BAD".
	Status string
	// Code is the response code without brackets or arguments, or "" if the
	// server sent none.
	Code ResponseCode
	// CodeArgs is the verbatim text between the code and the closing
	// bracket, or "" if the code had no arguments.
	CodeArgs string
	// Text is the human-readable text, with any response code removed.
	Text string

	_ struct{}
}
