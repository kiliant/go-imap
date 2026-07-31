package imapclient

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// NamespaceDescriptor describes one namespace prefix and its hierarchy
// delimiter. A zero Delimiter means the server reported NIL.
//
// Construct with keyed fields only; fields may be added in a future release.
type NamespaceDescriptor struct {
	Prefix    string
	Delimiter rune
	_         struct{}
}

// NamespaceData is returned by NAMESPACE (RFC 2342).
//
// Construct with keyed fields only; fields may be added in a future release.
type NamespaceData struct {
	Personal   []NamespaceDescriptor
	OtherUsers []NamespaceDescriptor
	Shared     []NamespaceDescriptor
	_          struct{}
}

// NamespaceCommand is an in-flight NAMESPACE command.
type NamespaceCommand struct {
	*Command
	data *NamespaceData
}

// Wait waits for NAMESPACE and returns the advertised namespace groups.
func (cmd *NamespaceCommand) Wait(ctx context.Context) (*NamespaceData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil namespace command")
	}
	if err := cmd.Command.Wait(ctx); err != nil {
		return nil, err
	}
	return cmd.data, nil
}

// Namespace requests namespace prefixes and hierarchy delimiters (RFC 2342).
func (c *Client) Namespace() *NamespaceCommand {
	data := &NamespaceData{}
	cmd := c.beginCommand("NAMESPACE", stateAuthenticated|stateSelected, nil, namespaceCollector(data))
	return &NamespaceCommand{Command: cmd, data: data}
}

func namespaceCollector(data *NamespaceData) commandCollector {
	return func(resp *untaggedResponse) (bool, error) {
		if resp.name != "NAMESPACE" || resp.hasNum || resp.cond != nil {
			return false, nil
		}
		if !resp.dec.ExpectSP() {
			return true, resp.dec.Err()
		}
		personal, err := decodeNamespaceGroup(resp.dec)
		if err != nil {
			return true, err
		}
		if !resp.dec.ExpectSP() {
			return true, resp.dec.Err()
		}
		otherUsers, err := decodeNamespaceGroup(resp.dec)
		if err != nil {
			return true, err
		}
		if !resp.dec.ExpectSP() {
			return true, resp.dec.Err()
		}
		shared, err := decodeNamespaceGroup(resp.dec)
		if err != nil {
			return true, err
		}
		if !resp.dec.ExpectCRLF() {
			return true, resp.dec.Err()
		}
		data.Personal, data.OtherUsers, data.Shared = personal, otherUsers, shared
		return true, nil
	}
}

func decodeNamespaceGroup(dec *imapwire.Decoder) ([]NamespaceDescriptor, error) {
	if dec.Special('(') {
		// Reuse ExpectList's robust nesting and separator handling by parsing
		// the outer list after restoring its opening token is impossible. The
		// opening parenthesis is therefore handled explicitly below.
		return decodeNamespaceDescriptorsAfterOpen(dec)
	}
	var nilGroup string
	if !dec.ExpectAstring(&nilGroup) {
		return nil, dec.Err()
	}
	if !strings.EqualFold(nilGroup, "NIL") {
		return nil, fmt.Errorf("invalid NAMESPACE group %q", nilGroup)
	}
	return nil, nil
}

func decodeNamespaceDescriptorsAfterOpen(dec *imapwire.Decoder) ([]NamespaceDescriptor, error) {
	var descriptors []NamespaceDescriptor
	if dec.Special(')') {
		return descriptors, nil
	}
	for {
		if !dec.ExpectSpecial('(') {
			return nil, dec.Err()
		}
		descriptor, err := decodeNamespaceDescriptorAfterOpen(dec)
		if err != nil {
			return nil, err
		}
		descriptors = append(descriptors, descriptor)
		if !dec.SP() {
			break
		}
	}
	if !dec.ExpectSpecial(')') {
		return nil, dec.Err()
	}
	return descriptors, nil
}

func decodeNamespaceDescriptorAfterOpen(dec *imapwire.Decoder) (NamespaceDescriptor, error) {
	var prefix string
	if !dec.ExpectMailbox(&prefix) || !dec.ExpectSP() {
		return NamespaceDescriptor{}, dec.Err()
	}
	var rawDelimiter string
	var nilDelimiter bool
	if !dec.ExpectNString(&rawDelimiter, &nilDelimiter) {
		return NamespaceDescriptor{}, dec.Err()
	}
	// RFC 2342 permits extension data after the delimiter. Preserve alignment
	// now; typed extension support can replace this with a parser later.
	for dec.SP() {
		if err := dec.DiscardValue(); err != nil {
			return NamespaceDescriptor{}, err
		}
	}
	if !dec.ExpectSpecial(')') {
		return NamespaceDescriptor{}, dec.Err()
	}
	var delimiter rune
	if !nilDelimiter {
		var size int
		delimiter, size = utf8.DecodeRuneInString(rawDelimiter)
		if delimiter == utf8.RuneError && size == 1 || size != len(rawDelimiter) {
			return NamespaceDescriptor{}, fmt.Errorf("invalid NAMESPACE hierarchy delimiter %q", rawDelimiter)
		}
	}
	return NamespaceDescriptor{Prefix: prefix, Delimiter: delimiter}, nil
}
