package imapwire

import "strings"

// Section specifiers, from the section-msgtext and section-text productions of
// RFC 3501 section 9. They are strings rather than an enumeration because a
// future extension may add one — BINARY already reuses the section syntax with a
// restricted body — and a closed set here would force a change on every layer
// above.
const (
	// SpecifierNone is the empty specifier: BODY[] or BODY[1.2], the whole
	// message or the whole part.
	SpecifierNone = ""
	// SpecifierHeader is BODY[HEADER]: the RFC 5322 header of the message or
	// part, including the blank line that ends it.
	SpecifierHeader = "HEADER"
	// SpecifierHeaderFields is BODY[HEADER.FIELDS (…)].
	SpecifierHeaderFields = "HEADER.FIELDS"
	// SpecifierHeaderFieldsNot is BODY[HEADER.FIELDS.NOT (…)].
	SpecifierHeaderFieldsNot = "HEADER.FIELDS.NOT"
	// SpecifierText is BODY[TEXT]: the body without the header.
	SpecifierText = "TEXT"
	// SpecifierMIME is BODY[1.MIME]: the MIME header of a part. It is only
	// legal after a section-part.
	SpecifierMIME = "MIME"
)

// maxSectionPartDepth caps the section-part nesting a caller may express or a
// server may send. The grammar sets no bound; 64 levels of nested MIME parts is
// already far past anything a mail client produces.
const maxSectionPartDepth = 64

// SectionPartial is the "<offset.count>" suffix of a body section.
//
// A FETCH command carries both numbers; the matching response carries only the
// offset, since the count is implied by the literal that follows. Count == 0
// therefore means "absent", which is unambiguous because the grammar makes it an
// nz-number.
type SectionPartial struct {
	Offset uint32
	Count  uint32
}

// BodySection is the section production of RFC 3501 section 9:
//
//	section       = "[" [section-spec] "]"
//	section-spec  = section-msgtext / (section-part ["." section-text])
//	section-part  = nz-number *("." nz-number)
//	header-list   = "(" header-fld-name *(SP header-fld-name) ")"
//
// It is a wire-level value: part numbers, a specifier string and header field
// names. Turning it into a fetch item is the job of the layer that owns that
// type.
type BodySection struct {
	// Part is the section-part, e.g. {1, 2} for BODY[1.2]. Nil for a section
	// that addresses the whole message.
	Part []uint32
	// Specifier is one of the Specifier constants, or an unrecognised name
	// from a future extension.
	Specifier string
	// Fields holds the header names of HEADER.FIELDS and HEADER.FIELDS.NOT.
	Fields []string
	// Partial is the "<offset[.count]>" suffix, if any.
	Partial *SectionPartial
}

// ExpectBodySection decodes a section, including any "<…>" partial suffix.
func (d *Decoder) ExpectBodySection(dst *BodySection) bool {
	if !d.ready("section") {
		return false
	}
	if !d.ExpectSpecial('[') {
		return false
	}
	*dst = BodySection{}

	b, ok := d.peek()
	if !ok {
		return d.failEOF("section")
	}
	switch {
	case b == ']':
		// BODY[]: the whole message.
	case isDigit(b):
		if !d.sectionPart(dst) {
			return false
		}
	default:
		if !d.sectionSpecifier(dst, false) {
			return false
		}
	}
	if !d.ExpectSpecial(']') {
		return false
	}
	return d.sectionPartial(dst)
}

// sectionPart decodes "nz-number *("." nz-number)" and, if a "." is followed by
// something that is not a digit, the section-text that ends it.
func (d *Decoder) sectionPart(dst *BodySection) bool {
	for {
		var n uint32
		if !d.ExpectNZNumber(&n) {
			return false
		}
		if len(dst.Part) >= maxSectionPartDepth {
			return d.fail("section-part", "more than %d part numbers", maxSectionPartDepth)
		}
		dst.Part = append(dst.Part, n)
		if !d.Special('.') {
			return true
		}
		b, ok := d.peek()
		if !ok {
			return d.failEOF("section-part")
		}
		if !isDigit(b) {
			// section-part "." section-text — MIME is legal only here.
			return d.sectionSpecifier(dst, true)
		}
	}
}

// sectionSpecifier decodes section-msgtext, plus MIME when allowMIME says a
// section-part preceded it.
func (d *Decoder) sectionSpecifier(dst *BodySection, allowMIME bool) bool {
	var name string
	if !d.ExpectAtom(&name) {
		return false
	}
	dst.Specifier = asciiUpper(name)
	switch dst.Specifier {
	case SpecifierHeaderFields, SpecifierHeaderFieldsNot:
		if !d.ExpectSP() {
			return false
		}
		fields := []string{}
		err := d.ExpectList(func() error {
			var f string
			if !d.ExpectAstring(&f) {
				return d.errOrSyntax("header-fld-name")
			}
			fields = append(fields, f)
			return nil
		})
		if err != nil {
			return false
		}
		if len(fields) == 0 {
			// header-list is 1*header-fld-name; an empty one would select
			// nothing, which no server means to say.
			return d.fail("header-list", "empty header field list")
		}
		dst.Fields = fields
	case SpecifierMIME:
		if !allowMIME {
			return d.fail("section-spec", "MIME requires a section-part")
		}
	case SpecifierHeader, SpecifierText:
	default:
		// An unknown specifier is kept verbatim rather than rejected: the
		// section syntax is an extension point, and the caller can still see
		// what it was.
	}
	return true
}

// sectionPartial decodes the optional "<" number ["." nz-number] ">" suffix.
func (d *Decoder) sectionPartial(dst *BodySection) bool {
	if !d.Special('<') {
		return true
	}
	var p SectionPartial
	if !d.ExpectNumber(&p.Offset) {
		return false
	}
	if d.Special('.') {
		if !d.ExpectNZNumber(&p.Count) {
			return false
		}
	}
	if !d.ExpectSpecial('>') {
		return false
	}
	dst.Partial = &p
	return true
}

// String renders the section in wire form, e.g. "[1.2.HEADER.FIELDS (From)]".
//
// It is a convenience for logs and tests. The wire path goes through
// [Encoder.BodySection], which shares the same code and can fall back to a
// literal for a header name that needs one.
func (s *BodySection) String() string {
	var sb strings.Builder
	e := NewEncoder(&sb, nil)
	e.BodySection(s)
	if err := e.Flush(); err != nil {
		return ""
	}
	return sb.String()
}
