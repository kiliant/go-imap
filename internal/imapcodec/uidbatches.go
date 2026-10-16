package imapcodec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ReadUIDBatchesCommand reads arguments after UIDBATCHES, including CRLF.
// Policy checks (minimum size, reversed range and maximum span) belong to
// the caller, so it can return the specified response code.
func ReadUIDBatchesCommand(dec *imapwire.Decoder) (uint32, imap.UIDBatchIndexRange, error) {
	var size uint32
	var indices imap.UIDBatchIndexRange
	if !dec.ExpectSP() || !dec.ExpectNumber(&size) {
		return 0, indices, dec.Err()
	}
	if size == 0 {
		return 0, indices, fmt.Errorf("zero UIDBATCHES size")
	}
	if dec.SP() {
		if !dec.ExpectNumber(&indices.First) || !dec.ExpectSpecial(':') || !dec.ExpectNumber(&indices.Last) {
			return 0, indices, dec.Err()
		}
		if indices.First == 0 || indices.Last == 0 {
			return 0, indices, fmt.Errorf("zero UIDBATCHES index")
		}
	}
	if !dec.ExpectCRLF() {
		return 0, indices, dec.Err()
	}
	return size, indices, nil
}

// WriteUIDBatchesCommand writes arguments after UIDBATCHES, without CRLF.
func WriteUIDBatchesCommand(enc *imapwire.Encoder, size uint32, indices imap.UIDBatchIndexRange) error {
	if size == 0 || (indices.First == 0) != (indices.Last == 0) {
		return fmt.Errorf("invalid UIDBATCHES arguments")
	}
	enc.SP().Number(size)
	if indices.First != 0 {
		enc.SP().Number(indices.First).Special(':').Number(indices.Last)
	}
	return enc.Err()
}

// ReadUIDBatchesResponse reads after the UIDBATCHES atom, including CRLF.
func ReadUIDBatchesResponse(dec *imapwire.Decoder) (*imap.UIDBatchesData, error) {
	data := &imap.UIDBatchesData{}
	if !dec.ExpectSP() || !dec.ExpectSpecial('(') {
		return nil, dec.Err()
	}
	var label string
	if !dec.ExpectAtom(&label) || !strings.EqualFold(label, "TAG") {
		return nil, fmt.Errorf("UIDBATCHES requires a TAG correlator")
	}
	if !dec.ExpectSP() || !dec.ExpectAstring(&data.Tag) || !dec.ExpectSpecial(')') {
		return nil, dec.Err()
	}
	if data.Tag == "" {
		return nil, fmt.Errorf("empty UIDBATCHES correlator")
	}
	if dec.SP() {
		var raw string
		if !dec.ExpectSequenceSet(&raw) {
			return nil, dec.Err()
		}
		for _, pair := range strings.Split(raw, ",") {
			ends := strings.Split(pair, ":")
			if len(ends) > 2 {
				return nil, fmt.Errorf("invalid UIDBATCHES range %q", pair)
			}
			first, err := uidBatchEndpoint(ends[0])
			if err != nil {
				return nil, err
			}
			last := first
			if len(ends) == 2 {
				last, err = uidBatchEndpoint(ends[1])
				if err != nil {
					return nil, err
				}
			}
			data.Batches = append(data.Batches, imap.UIDBatch{Range: imap.UIDRange{Start: first, Stop: last}})
		}
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return data, nil
}

func uidBatchEndpoint(raw string) (imap.UID, error) {
	// ParseUint accepts a leading '+', which the IMAP number grammar forbids.
	if raw == "" || strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, fmt.Errorf("invalid UIDBATCHES endpoint %q", raw)
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid UIDBATCHES endpoint %q", raw)
	}
	return imap.UID(n), nil
}

// WriteUIDBatchesResponse writes after the UIDBATCHES atom, including CRLF.
// A singleton is always written u:u, without NumRange.String normalization.
func WriteUIDBatchesResponse(enc *imapwire.Encoder, data *imap.UIDBatchesData) error {
	if data == nil || data.Tag == "" {
		return fmt.Errorf("UIDBATCHES requires a TAG correlator")
	}
	for _, batch := range data.Batches {
		if batch.Range.Start == 0 || batch.Range.Stop == 0 {
			return fmt.Errorf("zero UIDBATCHES endpoint")
		}
	}
	enc.SP().Special('(').Atom("TAG").SP().Astring(data.Tag).Special(')')
	for i, batch := range data.Batches {
		if i == 0 {
			enc.SP()
		} else {
			enc.Special(',')
		}
		enc.Number(uint32(batch.Range.Start)).Special(':').Number(uint32(batch.Range.Stop))
	}
	enc.CRLF()
	return enc.Err()
}
