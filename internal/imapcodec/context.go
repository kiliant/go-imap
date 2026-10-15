package imapcodec

import (
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ReadContextUpdateChanges decodes an ADDTO/REMOVEFROM position/set list,
// without expanding potentially enormous ranges or reordering its pairs.
func ReadContextUpdateChanges(dec *imapwire.Decoder, uid bool) ([]imap.ContextUpdateChange, error) {
	var changes []imap.ContextUpdateChange
	err := dec.ExpectList(func() error {
		var change imap.ContextUpdateChange
		var raw string
		if !dec.ExpectNumber(&change.Position) || !dec.ExpectSP() || !dec.ExpectAtom(&raw) {
			return dec.Err()
		}
		var err error
		if uid {
			change.UIDs, err = imap.ParseUIDSet(raw)
		} else {
			change.SeqNums, err = imap.ParseSeqSet(raw)
		}
		if err != nil {
			return err
		}
		changes = append(changes, change)
		return nil
	})
	if err == nil && len(changes) == 0 {
		err = fmt.Errorf("empty CONTEXT update list")
	}
	return changes, err
}

// WriteContextUpdateChanges is the inverse of ReadContextUpdateChanges.
// It uses the typed view; a caller preserving an item's exact spelling can
// instead emit its separately retained raw Value with Encoder.RawValue.
func WriteContextUpdateChanges(enc *imapwire.Encoder, uid bool, changes []imap.ContextUpdateChange) error {
	if len(changes) == 0 {
		return fmt.Errorf("empty CONTEXT update list")
	}
	for _, change := range changes {
		if uid && (len(change.UIDs) == 0 || len(change.SeqNums) != 0) ||
			!uid && (len(change.SeqNums) == 0 || len(change.UIDs) != 0) {
			return fmt.Errorf("CONTEXT update numbers do not match UID mode")
		}
	}
	enc.Special('(')
	for i, change := range changes {
		if i > 0 {
			enc.SP()
		}
		enc.Number(change.Position).SP()
		if uid {
			enc.RawValue([]byte(change.UIDs.String()))
		} else {
			enc.RawValue([]byte(change.SeqNums.String()))
		}
	}
	enc.Special(')')
	return enc.Err()
}
