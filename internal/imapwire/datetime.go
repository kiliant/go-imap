package imapwire

import "time"

// The two date shapes of RFC 3501 section 9.
//
//	date-time = DQUOTE date-day-fixed "-" date-month "-" date-year SP time SP
//	            zone DQUOTE
//	date      = date-text / DQUOTE date-text DQUOTE
//	date-text = date-day "-" date-month "-" date-year
//
// date-day-fixed is "(SP DIGIT) / 2DIGIT" — a space-padded day, not a
// zero-padded one, which is exactly what Go's "_2" reference layout means. The
// layouts below therefore parse " 7-Jul-1996", "07-Jul-1996" and "7-Jul-1996"
// alike, and format the padded form the grammar asks for.
const (
	dateTimeLayout = "_2-Jan-2006 15:04:05 -0700"
	dateLayout     = "_2-Jan-2006"
)

// ExpectDateTime decodes a date-time, as carried by INTERNALDATE and by the
// envelope date. The returned time keeps the offset the server sent rather than
// being converted to UTC: the offset is information about the message, and
// discarding it here would make it unrecoverable.
func (d *Decoder) ExpectDateTime(dst *time.Time) bool {
	if !d.ready("date-time") {
		return false
	}
	var s string
	if !d.ExpectQuoted(&s) {
		return false
	}
	t, err := time.Parse(dateTimeLayout, s)
	if err != nil {
		return d.fail("date-time", "malformed date-time %q", s)
	}
	*dst = t
	return true
}

// ExpectDate decodes a date, the day-granularity form used by SEARCH keys such
// as SINCE and BEFORE. Both the bare and the quoted spelling are accepted, as
// the grammar allows.
func (d *Decoder) ExpectDate(dst *time.Time) bool {
	if !d.ready("date") {
		return false
	}
	var s string
	if b, ok := d.peek(); ok && b == '"' {
		if !d.ExpectQuoted(&s) {
			return false
		}
	} else if !d.ExpectAtom(&s) {
		return false
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return d.fail("date", "malformed date %q", s)
	}
	*dst = t
	return true
}
