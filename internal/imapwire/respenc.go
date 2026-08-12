package imapwire

import "strings"

// BeginResponse writes the framing prefix for a response. The caller writes
// the response-specific payload and terminates it with [Encoder.CRLF].
func (e *Encoder) BeginResponse(kind ResponseKind, tag string) *Encoder {
	switch kind {
	case ResponseTagged:
		return e.Tag(tag).SP()
	case ResponseUntagged:
		return e.Special('*').SP()
	case ResponseContinuation:
		return e.Special('+')
	default:
		return e.fail("response", "unknown response kind %d", kind)
	}
}

// RespText writes a resp-text production. Code and Args are validated as one
// bracketed response code; Text is written verbatim as IMAP text.
func (e *Encoder) RespText(text RespText) *Encoder {
	if text.Code != "" {
		if strings.ContainsAny(text.Code, "[] \r\n") {
			return e.fail("resp-text-code", "invalid response code %q", text.Code)
		}
		if strings.ContainsAny(text.Args, "]\x00\r\n") {
			return e.fail("resp-text-code", "response-code arguments contain a closing bracket, NUL, or line break")
		}
		e.Special('[').Atom(strings.ToUpper(text.Code))
		if text.Args != "" {
			e.SP().rawString(text.Args)
		}
		e.Special(']')
		if text.Text != "" {
			e.SP()
		}
	}
	if strings.ContainsAny(text.Text, "\x00\r\n") {
		return e.fail("resp-text", "response text contains NUL or a line break")
	}
	return e.rawString(text.Text)
}

// RespCond writes a response condition and its resp-text. The caller supplies
// the framing prefix with [Encoder.BeginResponse] and the trailing CRLF.
func (e *Encoder) RespCond(cond RespCond) *Encoder {
	status := strings.ToUpper(cond.Status)
	switch status {
	case "OK", "NO", "BAD", "PREAUTH", "BYE":
	default:
		return e.fail("resp-cond", "unknown status condition %q", cond.Status)
	}
	e.Atom(status)
	return e.SP().RespText(cond.Text)
}

// ContinuationText writes the optional payload of a continuation response.
// The leading "+" is written with [Encoder.BeginResponse].
func (e *Encoder) ContinuationText(text string) *Encoder {
	if strings.ContainsAny(text, "\x00\r\n") {
		return e.fail("continue-req", "continuation text contains NUL or a line break")
	}
	if text != "" {
		e.SP().rawString(text)
	}
	return e
}

// ResponseLiteral announces a server-to-client literal and returns its payload
// writer. Unlike a command literal it never carries a non-synchronising marker
// and never waits for a continuation: the server is the side granting
// continuations, not the side waiting for one.
func (e *Encoder) ResponseLiteral(size int64, binary bool) (*LiteralWriter, error) {
	if e.err != nil {
		return nil, e.err
	}
	if e.lw != nil {
		e.fail("literal", "a literal is already open")
		return nil, e.err
	}
	if size < 0 {
		e.fail("literal", "negative literal size %d", size)
		return nil, e.err
	}
	if binary {
		e.rawString("~")
	}
	e.rawString("{").Number64(size).rawString("}\r\n")
	if e.err != nil {
		return nil, e.err
	}
	lw := &LiteralWriter{e: e, size: size, remaining: size, binary: binary}
	if size > 0 {
		e.lw = lw
	}
	return lw, nil
}
