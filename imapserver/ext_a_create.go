package imapserver

import (
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// CREATE-SPECIAL-USE (RFC 6154 section 3): the USE parameter on CREATE.
//
// The capability descriptor lives in ext_a_list.go beside SPECIAL-USE, which it
// depends on.

// createArgs carries CREATE's mailbox name and its optional USE parameter.
// CREATE shares parseMailbox with the other mailbox commands until a USE
// parameter appears, so the parser returns a plain string in the common case
// and this only when there is something extra to carry.
type createArgs struct {
	mailbox    string
	specialUse []imap.MailboxAttr
}

// parseCreate reads CREATE's mailbox name and the optional
// "(USE (\Archive ...))" parameter list of RFC 6154 section 3.
func parseCreate(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &createArgs{}
	if !decoder.ExpectMailbox(&args.mailbox) {
		return nil, 0, decoder.Err()
	}
	size := int64(len(args.mailbox))
	if decoder.SP() {
		if err := decoder.ExpectList(func() error {
			var parameter string
			if !decoder.ExpectAtom(&parameter) {
				return decoder.Err()
			}
			if !strings.EqualFold(parameter, "USE") {
				return fmt.Errorf("unsupported CREATE parameter %q", parameter)
			}
			if !decoder.ExpectSP() {
				return decoder.Err()
			}
			// Use attributes are flag-shaped ("\Archive"), and a backslash is
			// not an ATOM-CHAR, so they need the flag decoder rather than
			// ExpectAtom.
			var attrs []string
			if err := decoder.ExpectFlagList(&attrs); err != nil {
				return err
			}
			for _, attr := range attrs {
				args.specialUse = append(args.specialUse, imap.MailboxAttr(attr))
			}
			return nil
		}); err != nil {
			return nil, 0, err
		}
		for _, attr := range args.specialUse {
			size += int64(len(attr))
		}
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, size, nil
}

// createOptions validates the USE parameter and maps it onto CreateOptions.
//
// An unrecognised use attribute is refused rather than passed through: RFC 6154
// section 3 requires the USE response code on rejection, and a backend that
// silently created a mailbox without the requested use would leave the client
// believing it had one.
func createOptions(c *conn, args *createArgs) (*CreateOptions, error) {
	if len(args.specialUse) == 0 {
		return nil, nil
	}
	features := activeFeatures(&c.state, c.server)
	if !features[featureCreateSpecialUse] {
		return nil, &imap.Error{
			Type: imap.ErrorTypeNo,
			Code: imap.CodeUseAttr,
			Text: "CREATE does not accept a USE parameter",
		}
	}
	for _, attr := range args.specialUse {
		if !isSpecialUseAttr(attr) {
			return nil, &imap.Error{
				Type: imap.ErrorTypeNo,
				Code: imap.CodeUseAttr,
				Text: fmt.Sprintf("unsupported use attribute %q", string(attr)),
			}
		}
	}
	return &CreateOptions{SpecialUse: args.specialUse}, nil
}
