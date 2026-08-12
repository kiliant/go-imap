package imapcodec

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// WriteSearchCriteria writes criterion in the top-level SEARCH position.
func WriteSearchCriteria(enc *imapwire.Encoder, criterion imap.SearchCriteria) {
	if and, ok := criterion.(imap.SearchAnd); ok {
		if len(and) == 0 {
			enc.Atom("ALL")
			return
		}
		for i, x := range and {
			if i > 0 {
				enc.SP()
			}
			writeSearchKey(enc, x)
		}
		return
	}
	writeSearchKey(enc, criterion)
}

func writeSearchKey(enc *imapwire.Encoder, criterion imap.SearchCriteria) {
	if and, ok := criterion.(imap.SearchAnd); ok && len(and) != 1 {
		if len(and) == 0 {
			enc.Atom("ALL")
			return
		}
		enc.Special('(')
		for i, x := range and {
			if i > 0 {
				enc.SP()
			}
			writeSearchKey(enc, x)
		}
		enc.Special(')')
		return
	}
	switch c := criterion.(type) {
	case imap.SearchAnd:
		writeSearchKey(enc, c[0])
	case imap.SearchOr:
		enc.Atom("OR").SP()
		writeSearchKey(enc, c.Left)
		enc.SP()
		writeSearchKey(enc, c.Right)
	case imap.SearchNot:
		enc.Atom("NOT").SP()
		writeSearchKey(enc, c.Criteria)
	case imap.SearchKeyword:
		enc.Atom(string(c))
	case imap.SearchFlagKeyword:
		if c.Not {
			enc.Atom("UNKEYWORD")
		} else {
			enc.Atom("KEYWORD")
		}
		enc.SP().Flag(string(c.Flag))
	case imap.SearchHeaderField:
		enc.Atom("HEADER").SP().Astring(c.Field).SP().String(c.Value)
	case imap.SearchString:
		enc.Atom(string(c.Key)).SP().String(c.Value)
	case imap.SearchDate:
		enc.Atom(string(c.Key)).SP().Date(c.Date)
	case imap.SearchSize:
		enc.Atom(string(c.Key)).SP().Number64(c.Size)
	case imap.SearchSeqNum:
		writeSequenceSet(enc, c.Set.String())
	case imap.SearchUID:
		enc.Atom("UID").SP()
		writeSequenceSet(enc, c.Set.String())
	case imap.SearchSavedResult:
		enc.Atom("$")
	case imap.SearchWithin:
		enc.Atom(string(c.Key)).SP().Number64(c.Seconds)
	case imap.SearchObjectID:
		enc.Atom(string(c.Key)).SP().Astring(c.Value)
	case imap.SearchModSeq:
		enc.Atom("MODSEQ")
		if c.EntryName != "" {
			enc.SP().Astring(c.EntryName).SP().Atom(string(c.EntryType))
		}
		enc.SP().Number64(int64(c.ModSeq))
	case imap.SearchFuzzy:
		enc.Atom("FUZZY").SP()
		writeSearchKey(enc, c.Criteria)
	default:
		enc.Atom("")
	}
}

func writeSequenceSet(enc *imapwire.Encoder, set string) {
	if _, err := imap.ParseSeqSet(set); err != nil {
		enc.Atom("")
		return
	}
	enc.RawValue([]byte(set))
}

// ValidateSearchCriteria rejects a tree the semantic encoder cannot render.
func ValidateSearchCriteria(criterion imap.SearchCriteria) error {
	switch c := criterion.(type) {
	case imap.SearchAnd:
		for _, x := range c {
			if err := ValidateSearchCriteria(x); err != nil {
				return err
			}
		}
	case imap.SearchOr:
		if c.Left == nil || c.Right == nil {
			return fmt.Errorf("SEARCH OR requires both operands")
		}
		if err := ValidateSearchCriteria(c.Left); err != nil {
			return err
		}
		return ValidateSearchCriteria(c.Right)
	case imap.SearchNot:
		if c.Criteria == nil {
			return fmt.Errorf("SEARCH NOT requires an operand")
		}
		return ValidateSearchCriteria(c.Criteria)
	case imap.SearchFuzzy:
		if c.Criteria == nil {
			return fmt.Errorf("SEARCH FUZZY requires an operand")
		}
		return ValidateSearchCriteria(c.Criteria)
	case imap.SearchModSeq:
		if (c.EntryName == "") != (c.EntryType == "") {
			return fmt.Errorf("SEARCH MODSEQ requires an entry name and entry type together")
		}
		if c.ModSeq > 1<<63-1 {
			return fmt.Errorf("SEARCH MODSEQ value exceeds 63 bits")
		}
	case imap.SearchKeyword, imap.SearchFlagKeyword, imap.SearchHeaderField,
		imap.SearchString, imap.SearchDate, imap.SearchSize, imap.SearchSeqNum,
		imap.SearchUID, imap.SearchSavedResult, imap.SearchWithin, imap.SearchObjectID:
	default:
		return fmt.Errorf("unsupported SEARCH criteria type %T", criterion)
	}
	return nil
}

// ReadSearchCriteria decodes the remaining search-key sequence. It consumes
// keys and their separating spaces, but not the command's trailing CRLF.
func ReadSearchCriteria(dec *imapwire.Decoder) (imap.SearchCriteria, error) {
	first, err := readSearchKey(dec)
	if err != nil {
		return nil, err
	}
	items := []imap.SearchCriteria{first}
	for dec.SP() {
		next, err := readSearchKey(dec)
		if err != nil {
			return nil, err
		}
		items = append(items, next)
	}
	if len(items) == 1 {
		return items[0], nil
	}
	return imap.SearchAnd(items), nil
}

func readSearchKey(dec *imapwire.Decoder) (imap.SearchCriteria, error) {
	if dec.PeekSpecial('(') {
		var items []imap.SearchCriteria
		err := dec.ExpectList(func() error {
			item, err := readSearchKey(dec)
			if err == nil {
				items = append(items, item)
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("empty parenthesised SEARCH key")
		}
		if len(items) == 1 {
			return items[0], nil
		}
		return imap.SearchAnd(items), nil
	}
	var rawSet string
	if dec.SequenceSet(&rawSet) {
		set, err := imap.ParseSeqSet(rawSet)
		if err != nil {
			return nil, err
		}
		return imap.SearchSeqNum{Set: set}, nil
	}

	var atom string
	if !dec.ExpectAstring(&atom) {
		return nil, dec.Err()
	}
	upper := strings.ToUpper(atom)
	switch upper {
	case "OR":
		left, err := readRequiredSearchKey(dec)
		if err != nil {
			return nil, err
		}
		right, err := readRequiredSearchKey(dec)
		if err != nil {
			return nil, err
		}
		return imap.SearchOr{Left: left, Right: right}, nil
	case "NOT":
		item, err := readRequiredSearchKey(dec)
		return imap.SearchNot{Criteria: item}, err
	case "FUZZY":
		item, err := readRequiredSearchKey(dec)
		return imap.SearchFuzzy{Criteria: item}, err
	case "KEYWORD", "UNKEYWORD":
		if !dec.ExpectSP() {
			return nil, dec.Err()
		}
		var flag string
		if !dec.ExpectFlag(&flag) {
			return nil, dec.Err()
		}
		return imap.SearchFlagKeyword{Flag: imap.Flag(flag), Not: upper == "UNKEYWORD"}, nil
	case "HEADER":
		if !dec.ExpectSP() {
			return nil, dec.Err()
		}
		var field, value string
		if !dec.ExpectAstring(&field) || !dec.ExpectSP() || !dec.ExpectString(&value) {
			return nil, dec.Err()
		}
		return imap.SearchHeaderField{Field: field, Value: value}, nil
	case "BCC", "BODY", "CC", "FROM", "SUBJECT", "TEXT", "TO":
		var value string
		if !dec.ExpectSP() || !dec.ExpectString(&value) {
			return nil, dec.Err()
		}
		return imap.SearchString{Key: imap.SearchStringKey(upper), Value: value}, nil
	case "BEFORE", "ON", "SINCE", "SENTBEFORE", "SENTON", "SENTSINCE", "SAVEDBEFORE", "SAVEDON", "SAVEDSINCE":
		var date time.Time
		if !dec.ExpectSP() || !dec.ExpectDate(&date) {
			return nil, dec.Err()
		}
		return imap.SearchDate{Key: imap.SearchDateKey(upper), Date: date}, nil
	case "LARGER", "SMALLER":
		var size int64
		if !dec.ExpectSP() || !dec.ExpectNumber64(&size) {
			return nil, dec.Err()
		}
		return imap.SearchSize{Key: imap.SearchSizeKey(upper), Size: size}, nil
	case "UID":
		var raw string
		if !dec.ExpectSP() || !dec.ExpectSequenceSet(&raw) {
			return nil, dec.Err()
		}
		set, err := imap.ParseUIDSet(raw)
		if err != nil {
			return nil, err
		}
		return imap.SearchUID{Set: set}, nil
	case "$":
		return imap.SearchSavedResult{}, nil
	case "OLDER", "YOUNGER":
		var seconds int64
		if !dec.ExpectSP() || !dec.ExpectNumber64(&seconds) {
			return nil, dec.Err()
		}
		return imap.SearchWithin{Key: imap.SearchWithinKey(upper), Seconds: seconds}, nil
	case "EMAILID", "THREADID":
		var value string
		if !dec.ExpectSP() || !dec.ExpectAstring(&value) {
			return nil, dec.Err()
		}
		return imap.SearchObjectID{Key: imap.SearchObjectIDKey(upper), Value: value}, nil
	case "MODSEQ":
		return readSearchModSeq(dec)
	}
	return imap.SearchKeyword(upper), nil
}

func readRequiredSearchKey(dec *imapwire.Decoder) (imap.SearchCriteria, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	return readSearchKey(dec)
}

func readSearchModSeq(dec *imapwire.Decoder) (imap.SearchCriteria, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var first string
	if !dec.ExpectAstring(&first) {
		return nil, dec.Err()
	}
	if n, err := strconv.ParseUint(first, 10, 63); err == nil {
		return imap.SearchModSeq{ModSeq: n}, nil
	}
	var entryType string
	var n int64
	if !dec.ExpectSP() || !dec.ExpectAtom(&entryType) || !dec.ExpectSP() || !dec.ExpectNumber64(&n) {
		return nil, dec.Err()
	}
	return imap.SearchModSeq{ModSeq: uint64(n), EntryName: first, EntryType: imap.SearchModSeqMetadata(entryType)}, nil
}
