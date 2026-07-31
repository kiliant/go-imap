package imapwire

// Character classes from the RFC 3501 section 9 formal syntax:
//
//	CHAR            = %x01-7F
//	CTL             = %x00-1F / %x7F
//	atom-specials   = "(" / ")" / "{" / SP / CTL / list-wildcards /
//	                  quoted-specials / resp-specials
//	list-wildcards  = "%" / "*"
//	quoted-specials = DQUOTE / "\"
//	resp-specials   = "]"
//	ATOM-CHAR       = <any CHAR except atom-specials>
//	ASTRING-CHAR    = ATOM-CHAR / resp-specials
//	list-char       = ATOM-CHAR / list-wildcards / resp-specials
//	TEXT-CHAR       = <any CHAR except CR and LF>
//	QUOTED-CHAR     = <any TEXT-CHAR except quoted-specials> /
//	                  "\" quoted-specials
//
// Deliberate deviation, decoding side: octets 0x80-0xFF are accepted wherever
// ATOM-CHAR is expected, even though CHAR is 7-bit. Servers send raw UTF-8 in
// atoms — mailbox names in particular — with and without UTF8=ACCEPT, and RFC
// 9051 adds UTF8-2/3/4 to several productions. Rejecting those bytes makes real
// mailboxes unreachable while preventing no attack, since the byte count is
// bounded by the line-length limit either way. The encoding side stays strict:
// it never emits an 8-bit byte outside a literal.

// isCTL reports whether b is a control octet. NUL is included.
func isCTL(b byte) bool { return b <= 0x1f || b == 0x7f }

// isAtomChar implements ATOM-CHAR, extended to 8-bit octets as described above.
func isAtomChar(b byte) bool {
	switch b {
	case '(', ')', '{', ' ', '%', '*', '"', '\\', ']':
		return false
	}
	return !isCTL(b)
}

// isAstringChar implements ASTRING-CHAR: ATOM-CHAR plus the resp-special "]".
// The distinction matters inside resp-text-code, where "]" terminates the code
// and therefore may not be swallowed by an atom.
func isAstringChar(b byte) bool { return b == ']' || isAtomChar(b) }

// isListChar implements list-char, used by the list-mailbox production: an
// astring-char plus the list wildcards "%" and "*", which must survive
// unquoted so that LIST patterns keep their meaning.
func isListChar(b byte) bool { return b == '%' || b == '*' || isAstringChar(b) }

// isTextChar implements TEXT-CHAR, again extended to 8-bit octets. NUL is
// excluded: it terminates nothing in IMAP but has no legitimate use either, and
// letting it through invites downstream C-string confusion.
func isTextChar(b byte) bool { return b != 0 && b != '\r' && b != '\n' }

// isQuotedChar reports whether b may appear unescaped inside a quoted string.
func isQuotedChar(b byte) bool { return isTextChar(b) && b != '"' && b != '\\' }

// isDigit reports whether b is an ASCII digit.
func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// canBeAtom reports whether s may be written as a bare atom. Used by the
// encoder to pick the shortest legal representation.
//
// The value "NIL" is excluded on purpose: written as an atom it is
// indistinguishable from the nil token of the nstring production, and servers
// have been observed to treat it as such even in astring position.
func canBeAtom(s string, class func(byte) bool) bool {
	if s == "" || equalFold(s, "NIL") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !class(s[i]) || s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// canBeQuoted reports whether s may be written as a quoted string. 8-bit octets
// are excluded: RFC 3501 restricts QUOTED-CHAR to CHAR, so they must become a
// literal.
func canBeQuoted(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 || !isTextChar(s[i]) {
			return false
		}
	}
	return true
}

// equalFold compares two strings under ASCII case folding. strings.EqualFold
// would also fold non-ASCII, which IMAP keyword comparison must not do.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if toUpper(a[i]) != toUpper(b[i]) {
			return false
		}
	}
	return true
}

func toUpper(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - 'a' + 'A'
	}
	return b
}
