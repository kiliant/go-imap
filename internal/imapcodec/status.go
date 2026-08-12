package imapcodec

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ReadStatusItems decodes the parenthesised item list of a STATUS command.
func ReadStatusItems(dec *imapwire.Decoder) ([]imap.StatusItemKeyword, error) {
	var items []imap.StatusItemKeyword
	err := dec.ExpectList(func() error {
		var item string
		if !dec.ExpectAtom(&item) {
			return dec.Err()
		}
		items = append(items, imap.StatusItemKeyword(strings.ToUpper(item)))
		return nil
	})
	return items, err
}

// WriteStatusItems encodes a parenthesised STATUS item list.
func WriteStatusItems(enc *imapwire.Encoder, items []imap.StatusItemKeyword) {
	enc.List(len(items), func(i int) { enc.Atom(string(items[i])) })
}

// ReadStatusResponse decodes the part of an untagged STATUS response following
// its STATUS atom, including the leading space and trailing CRLF.
func ReadStatusResponse(dec *imapwire.Decoder) (*imap.StatusData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	data := &imap.StatusData{Values: make(map[imap.StatusItemKeyword]any)}
	if !dec.ExpectMailbox(&data.Mailbox) || !dec.ExpectSP() {
		return nil, dec.Err()
	}
	err := dec.ExpectList(func() error {
		var item string
		if !dec.ExpectAtom(&item) || !dec.ExpectSP() {
			return dec.Err()
		}
		key := imap.StatusItemKeyword(strings.ToUpper(item))
		if key == imap.StatusItemMailboxID {
			id, err := ReadFetchObjectID(dec)
			if err != nil {
				return err
			}
			data.Values[key] = id
			return nil
		}
		var raw string
		if !dec.ExpectAstring(&raw) {
			return dec.Err()
		}
		if value, err := strconv.ParseUint(raw, 10, 64); err == nil {
			data.Values[key] = value
		} else {
			data.Values[key] = raw
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	if err := projectStatus(data); err != nil {
		return nil, err
	}
	return data, nil
}

func projectStatus(data *imap.StatusData) error {
	for item, value := range data.Values {
		number, numeric := value.(uint64)
		switch item {
		case imap.StatusItemMessages, imap.StatusItemUIDNext, imap.StatusItemUIDValidity, imap.StatusItemUnseen, imap.StatusItemRecent:
			if !numeric || number > uint64(^uint32(0)) {
				return fmt.Errorf("invalid numeric STATUS value for %s", item)
			}
			switch item {
			case imap.StatusItemMessages:
				data.NumMessages = uint32(number)
			case imap.StatusItemUIDNext:
				data.UIDNext = imap.UID(number)
			case imap.StatusItemUIDValidity:
				data.UIDValidity = uint32(number)
			case imap.StatusItemUnseen:
				data.NumUnseen = uint32(number)
			case imap.StatusItemRecent:
				data.NumRecent = uint32(number)
			}
		case imap.StatusItemHighestModSeq:
			if !numeric {
				return fmt.Errorf("invalid numeric STATUS value for %s", item)
			}
			data.HighestModSeq = number
		}
	}
	return nil
}

// WriteStatusResponse writes one complete untagged STATUS response. Values is
// authoritative; convenience fields are not emitted unless present in Values.
func WriteStatusResponse(enc *imapwire.Encoder, data *imap.StatusData) error {
	if data == nil || data.Mailbox == "" {
		return fmt.Errorf("STATUS response requires a mailbox")
	}
	keys := make([]string, 0, len(data.Values))
	for key := range data.Values {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	enc.BeginResponse(imapwire.ResponseUntagged, "").Atom("STATUS").SP().Mailbox(data.Mailbox).SP().Special('(')
	for i, rawKey := range keys {
		if i > 0 {
			enc.SP()
		}
		key := imap.StatusItemKeyword(rawKey)
		enc.Atom(rawKey).SP()
		value := data.Values[key]
		switch value := value.(type) {
		case uint64:
			if value > 1<<63-1 {
				return fmt.Errorf("STATUS %s exceeds number64", key)
			}
			enc.Number64(int64(value))
		case string:
			if key == imap.StatusItemMailboxID {
				if err := validateObjectID(value); err != nil {
					return err
				}
				enc.List(1, func(int) { enc.Atom(value) })
			} else if strings.EqualFold(value, "NIL") {
				enc.NIL()
			} else {
				enc.Astring(value)
			}
		default:
			return fmt.Errorf("unsupported STATUS %s value type %T", key, value)
		}
	}
	enc.Special(')').CRLF()
	return enc.Err()
}
