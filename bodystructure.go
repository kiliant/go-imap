package imap

import "strings"

// BodyStructure is the MIME structure of a message as reported by the BODY and
// BODYSTRUCTURE fetch items. RFC 3501 section 7.4.2, RFC 9051 section 7.5.2.
//
// It is a marker interface with an unexported method, so the set of body
// structure shapes is open to this library and closed to external
// implementers. That is what allows methods to be added here without breaking
// anyone. The two implementations are [*BodyStructureSinglePart] and
// [*BodyStructureMultiPart]; switch on the concrete type, and treat an
// unrecognised type as an opaque part rather than panicking, so that a future
// addition does not break the switch.
type BodyStructure interface {
	// MediaType returns the "type/subtype" of the part, lower-cased.
	MediaType() string

	// Walk calls f for the part and, depth first, for every descendant. See
	// [BodyStructureWalkFunc].
	Walk(f BodyStructureWalkFunc)

	// Disposition returns the parsed Content-Disposition of the part, or nil
	// if the server did not report one. RFC 2183.
	Disposition() *BodyStructureDisposition

	bodyStructure()
}

// BodyStructureWalkFunc is called by [BodyStructure.Walk] for each part. path
// is the IMAP body part number of the part — the sequence of 1-based indices
// that addresses it in a BODY[...] section specifier, empty for the root. RFC
// 3501 section 6.4.5.
//
// Returning false skips the part's children.
type BodyStructureWalkFunc func(path []int, part BodyStructure) (walkChildren bool)

// BodyStructureSinglePart is a non-multipart MIME part. RFC 3501 section 9,
// production body-type-1part.
//
// Construct with keyed fields only; fields may be added in a future release.
type BodyStructureSinglePart struct {
	// Type and Subtype are the two halves of the Content-Type, for example
	// "text" and "plain". Case is as the server sent it.
	Type    string
	Subtype string

	// Params are the Content-Type parameters, with RFC 2231 continuations
	// already assembled and names lower-cased. See [DecodeParams].
	Params map[string]string

	// ID is the Content-ID, Description the decoded Content-Description and
	// Encoding the Content-Transfer-Encoding, for example "base64". Any may
	// be empty.
	ID          string
	Description string
	Encoding    string

	// Size is the size of the encoded part in octets.
	Size uint32

	// Text is set when Type is "text", and carries the extra field the
	// grammar defines for text parts.
	Text *BodyStructureText

	// Message is set when the media type is "message/rfc822", and carries
	// the encapsulated message's envelope and structure.
	Message *BodyStructureMessageRFC822

	// Extended holds the optional extension fields, present only when the
	// server was asked for BODYSTRUCTURE rather than BODY, and even then
	// only as far as the server chose to send them.
	Extended *BodyStructureSinglePartExt

	_ struct{}
}

// BodyStructureText carries the field the grammar adds for a text part.
// RFC 3501 section 9, production body-type-text.
//
// Construct with keyed fields only; fields may be added in a future release.
type BodyStructureText struct {
	// NumLines is the number of lines in the part.
	NumLines int64

	_ struct{}
}

// BodyStructureMessageRFC822 carries the fields the grammar adds for a
// message/rfc822 part. RFC 3501 section 9, production body-type-msg.
//
// Construct with keyed fields only; fields may be added in a future release.
type BodyStructureMessageRFC822 struct {
	// Envelope is the encapsulated message's envelope.
	Envelope *Envelope
	// BodyStructure is the encapsulated message's structure.
	BodyStructure BodyStructure
	// NumLines is the number of lines in the encapsulated message.
	NumLines int64

	_ struct{}
}

// BodyStructureSinglePartExt holds the optional extension fields of a
// single-part body structure. RFC 3501 section 9, production body-ext-1part.
//
// The fields are positional on the wire: a server that sends Location must send
// everything before it. A nil pointer or empty value therefore means "not
// reported", never "reported as empty".
//
// Construct with keyed fields only; fields may be added in a future release.
type BodyStructureSinglePartExt struct {
	// MD5 is the Content-MD5 of the part. RFC 1864.
	MD5 string
	// Disp is the parsed Content-Disposition. RFC 2183.
	Disp *BodyStructureDisposition
	// Lang holds the Content-Language tags. RFC 3282.
	Lang []string
	// Location is the Content-Location URI. RFC 2557.
	Location string

	_ struct{}
}

// BodyStructureMultiPart is a multipart MIME part. RFC 3501 section 9,
// production body-type-mpart.
//
// Construct with keyed fields only; fields may be added in a future release.
type BodyStructureMultiPart struct {
	// Children are the nested parts, in order. IMAP numbers them from 1.
	Children []BodyStructure

	// Subtype is the multipart subtype, for example "mixed" or
	// "alternative". The type is always "multipart".
	Subtype string

	// Extended holds the optional extension fields; see
	// [BodyStructureSinglePart.Extended].
	Extended *BodyStructureMultiPartExt

	_ struct{}
}

// BodyStructureMultiPartExt holds the optional extension fields of a multipart
// body structure. RFC 3501 section 9, production body-ext-mpart.
//
// Construct with keyed fields only; fields may be added in a future release.
type BodyStructureMultiPartExt struct {
	// Params are the Content-Type parameters, notably "boundary".
	Params map[string]string
	// Disp is the parsed Content-Disposition. RFC 2183.
	Disp *BodyStructureDisposition
	// Lang holds the Content-Language tags. RFC 3282.
	Lang []string
	// Location is the Content-Location URI. RFC 2557.
	Location string

	_ struct{}
}

// BodyStructureDisposition is a parsed Content-Disposition header. RFC 2183.
//
// Construct with keyed fields only; fields may be added in a future release.
type BodyStructureDisposition struct {
	// Value is the disposition type, for example "inline" or "attachment".
	Value string
	// Params are the disposition parameters, with RFC 2231 continuations
	// already assembled and names lower-cased. See [DecodeParams].
	Params map[string]string

	_ struct{}
}

func (*BodyStructureSinglePart) bodyStructure() {}
func (*BodyStructureMultiPart) bodyStructure()  {}

// MediaType returns the "type/subtype" of the part, lower-cased.
func (bs *BodyStructureSinglePart) MediaType() string {
	return strings.ToLower(bs.Type) + "/" + strings.ToLower(bs.Subtype)
}

// MediaType returns the "multipart/subtype" of the part, lower-cased.
func (bs *BodyStructureMultiPart) MediaType() string {
	return "multipart/" + strings.ToLower(bs.Subtype)
}

// Disposition returns the parsed Content-Disposition of the part, or nil if the
// server did not report one.
func (bs *BodyStructureSinglePart) Disposition() *BodyStructureDisposition {
	if bs.Extended == nil {
		return nil
	}
	return bs.Extended.Disp
}

// Disposition returns the parsed Content-Disposition of the part, or nil if the
// server did not report one.
func (bs *BodyStructureMultiPart) Disposition() *BodyStructureDisposition {
	if bs.Extended == nil {
		return nil
	}
	return bs.Extended.Disp
}

// Filename returns the part's suggested filename: the "filename" disposition
// parameter, falling back to the "name" Content-Type parameter, which
// pre-RFC 2183 mailers used. It returns "" if neither is present.
func (bs *BodyStructureSinglePart) Filename() string {
	if d := bs.Disposition(); d != nil {
		if v, ok := lookupParam(d.Params, "filename"); ok {
			return v
		}
	}
	if v, ok := lookupParam(bs.Params, "name"); ok {
		return v
	}
	return ""
}

// Filename returns the part's suggested filename from its Content-Disposition,
// or "" if it has none.
func (bs *BodyStructureMultiPart) Filename() string {
	if d := bs.Disposition(); d != nil {
		if v, ok := lookupParam(d.Params, "filename"); ok {
			return v
		}
	}
	return ""
}

func lookupParam(params map[string]string, name string) (string, bool) {
	if v, ok := params[name]; ok {
		return v, v != ""
	}
	for k, v := range params {
		if strings.EqualFold(k, name) {
			return v, v != ""
		}
	}
	return "", false
}

// Walk calls f for the part and, depth first, for every descendant.
func (bs *BodyStructureSinglePart) Walk(f BodyStructureWalkFunc) { walkBody(nil, bs, f) }

// Walk calls f for the part and, depth first, for every descendant.
func (bs *BodyStructureMultiPart) Walk(f BodyStructureWalkFunc) { walkBody(nil, bs, f) }

func walkBody(path []int, bs BodyStructure, f BodyStructureWalkFunc) {
	if !f(path, bs) {
		return
	}
	mp, ok := bs.(*BodyStructureMultiPart)
	if !ok {
		// A message/rfc822 part addresses its encapsulated body with the
		// same part number as itself (RFC 3501 section 6.4.5), so its
		// structure is not a separate node in the path space.
		if sp, ok := bs.(*BodyStructureSinglePart); ok && sp.Message != nil && sp.Message.BodyStructure != nil {
			if inner, ok := sp.Message.BodyStructure.(*BodyStructureMultiPart); ok {
				walkChildren(path, inner, f)
			}
		}
		return
	}
	walkChildren(path, mp, f)
}

func walkChildren(path []int, mp *BodyStructureMultiPart, f BodyStructureWalkFunc) {
	for i, child := range mp.Children {
		childPath := make([]int, len(path)+1)
		copy(childPath, path)
		childPath[len(path)] = i + 1
		walkBody(childPath, child, f)
	}
}
