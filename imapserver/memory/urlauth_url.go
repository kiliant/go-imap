package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// The parsed fields are for validation and lookup only. rump retains the exact
// supplied bytes for MAC verification, including escape spelling and case.
type urlAuthURL struct {
	rump, owner, mailbox string
	access, identity     string
	uid                  imap.UID
	uidValidity          uint32
	section              *imap.FetchItemBodySection
	token                []byte
}

func parseURLAuthURL(raw string, full bool, now time.Time) (*urlAuthURL, bool) {
	for _, b := range []byte(raw) {
		if b <= ' ' || b >= 0x7f || strings.ContainsRune("\"<>\\^`{|}", rune(b)) {
			return nil, false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "imap") || u.Opaque != "" ||
		u.Hostname() == "" || strings.ContainsAny(u.Hostname(), ";/@%") || u.User == nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(raw, "#") {
		return nil, false
	}
	if _, password := u.User.Password(); password {
		return nil, false
	}
	_, rest, _ := strings.Cut(raw, "://")
	authority, _, found := strings.Cut(rest, "/")
	if !found || strings.Count(authority, "@") != 1 {
		return nil, false
	}
	user, _, _ := strings.Cut(authority, "@")
	if owner, auth, hasAuth := strings.Cut(user, ";"); hasAuth {
		if !strings.HasPrefix(strings.ToUpper(auth), "AUTH=") || len(auth) == 5 || strings.Contains(auth[5:], ";") {
			return nil, false
		}
		mechanism, ok := urlAuthComponent(auth[5:], ";/:@")
		if !ok || len(mechanism) > 20 {
			return nil, false
		}
		if mechanism != "*" {
			for _, b := range []byte(strings.ToUpper(mechanism)) {
				if !(b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_') {
					return nil, false
				}
			}
		}
		user = owner
	}
	owner, ok := urlAuthComponent(user, ";/:@")
	if !ok {
		return nil, false
	}

	at := strings.LastIndex(strings.ToUpper(raw), ";URLAUTH=")
	if at < 0 {
		return nil, false
	}
	prefix, accessText := raw[:at], raw[at+len(";URLAUTH="):]
	result := &urlAuthURL{rump: raw, owner: owner, section: &imap.FetchItemBodySection{Peek: true}}
	if full {
		parts := strings.Split(accessText, ":")
		if len(parts) != 3 || !strings.EqualFold(parts[1], "INTERNAL") {
			return nil, false
		}
		result.token, err = hex.DecodeString(parts[2])
		if err != nil || len(result.token) != sha256.Size {
			return nil, false
		}
		result.rump = raw[:len(raw)-len(parts[1])-len(parts[2])-2]
		accessText = parts[0]
	}
	if strings.ContainsAny(accessText, ":/;") || accessText == "" {
		return nil, false
	}
	lower := strings.ToLower(accessText)
	switch {
	case lower == "anonymous", lower == "authuser":
		result.access = lower
	case strings.HasPrefix(lower, "user+"), strings.HasPrefix(lower, "submit+"):
		kind, identity, _ := strings.Cut(accessText, "+")
		result.access = strings.ToLower(kind)
		result.identity, ok = urlAuthComponent(identity, ";/:@")
		if !ok {
			return nil, false
		}
	default:
		return nil, false
	}
	if at := strings.LastIndex(strings.ToUpper(prefix), ";EXPIRE="); at >= 0 {
		expiry, err := time.Parse(time.RFC3339, prefix[at+len(";EXPIRE="):])
		if err != nil || !now.Before(expiry) {
			return nil, false
		}
		prefix = prefix[:at]
	}
	// Reparse only the resource portion. Never serialize it back into rump.
	resource, err := url.Parse(prefix)
	if err != nil {
		return nil, false
	}
	path, ok := strings.CutPrefix(resource.EscapedPath(), "/")
	if !ok {
		return nil, false
	}
	uidAt := strings.Index(strings.ToUpper(path), "/;UID=")
	if uidAt < 0 {
		return nil, false
	}
	mailboxText, tail := path[:uidAt], path[uidAt+len("/;UID="):]
	if mailbox, validity, hasValidity := strings.Cut(mailboxText, ";"); hasValidity {
		if !strings.HasPrefix(strings.ToUpper(validity), "UIDVALIDITY=") {
			return nil, false
		}
		n, ok := urlAuthNumber(validity[len("UIDVALIDITY="):], true)
		if !ok {
			return nil, false
		}
		result.uidValidity = n
		mailboxText = mailbox
	}
	result.mailbox, ok = urlAuthComponent(mailboxText, ";")
	if !ok {
		return nil, false
	}
	result.mailbox = mailboxKey(result.mailbox)
	parts := strings.Split(tail, "/")
	n, ok := urlAuthNumber(parts[0], true)
	if !ok {
		return nil, false
	}
	result.uid = imap.UID(n)
	parts = parts[1:]
	if len(parts) > 0 && strings.HasPrefix(strings.ToUpper(parts[0]), ";SECTION=") {
		section, err := url.PathUnescape(parts[0][len(";SECTION="):])
		if err != nil || section == "" || strings.ContainsAny(section, "\x00\r\n[]") {
			return nil, false
		}
		decoder := imapwire.NewDecoderString("["+section+"]", nil)
		var wire imapwire.BodySection
		if !decoder.ExpectBodySection(&wire) || !decoder.AtEOF() || wire.Partial != nil {
			return nil, false
		}
		for _, number := range wire.Part {
			if uint64(number) > uint64(^uint(0)>>1) {
				return nil, false
			}
			result.section.Part = append(result.section.Part, int(number))
		}
		switch wire.Specifier {
		case imapwire.SpecifierHeaderFields:
			result.section.Specifier, result.section.HeaderFields = imap.PartSpecifierHeader, wire.Fields
		case imapwire.SpecifierHeaderFieldsNot:
			result.section.Specifier, result.section.HeaderFieldsNot = imap.PartSpecifierHeader, wire.Fields
		default:
			result.section.Specifier = imap.PartSpecifier(wire.Specifier)
		}
		parts = parts[1:]
	}
	if len(parts) > 0 && strings.HasPrefix(strings.ToUpper(parts[0]), ";PARTIAL=") {
		offsetText, sizeText, hasSize := strings.Cut(parts[0][len(";PARTIAL="):], ".")
		offset, ok := urlAuthNumber(offsetText, false)
		if !ok {
			return nil, false
		}
		size := int64(1<<63 - 1)
		if hasSize {
			count, ok := urlAuthNumber(sizeText, true)
			if !ok {
				return nil, false
			}
			size = int64(count)
		}
		result.section.Partial = &imap.SectionPartial{Offset: int64(offset), Size: size}
		parts = parts[1:]
	}
	if len(parts) != 0 {
		return nil, false
	}
	return result, true
}

func urlAuthComponent(raw, forbidden string) (string, bool) {
	if raw == "" || strings.ContainsAny(raw, forbidden) {
		return "", false
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || !utf8.ValidString(decoded) || strings.ContainsAny(decoded, "\x00\r\n") {
		return "", false
	}
	return decoded, true
}

func urlAuthNumber(raw string, nonzero bool) (uint32, bool) {
	if raw == "" || nonzero && raw[0] == '0' {
		return 0, false
	}
	for _, b := range []byte(raw) {
		if b < '0' || b > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	return uint32(n), err == nil
}
