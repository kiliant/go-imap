package imapclient

// Extension group B: synchronisation and identity.
//
// This file holds the vocabulary shared by CONDSTORE and QRESYNC (RFC 7162),
// OBJECTID (RFC 8474), SAVEDATE (RFC 8514), STATUS=SIZE (RFC 8438),
// APPENDLIMIT (RFC 7889), PREVIEW (RFC 8970) and REPLACE (RFC 8508).
//
// The methods added by this group are spelled with a "Sync" suffix —
// [Client.SelectSync], [Client.FetchUIDSync], [Client.StoreUIDSync] — where
// they are the synchronisation-aware form of a base-protocol command. "Sync"
// refers to mailbox synchronisation, not to blocking behaviour: these methods
// follow exactly the same command-handle shape as the base commands they
// extend.

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// MaxModSeq is the largest legal mod-sequence value.
//
// RFC 7162 section 7 defines mod-sequence-value as a "positive unsigned 63-bit
// integer (1 <= n <= 9,223,372,036,854,775,807)". RFC 4551 originally defined
// mod-sequences as unsigned 64-bit values; RFC 7162 section 3.1 deliberately
// narrowed that to 63 bits so implementations on platforms without an unsigned
// 64-bit type remain possible. This library carries mod-sequences as uint64 —
// never anything narrower — and range-checks them against this constant before
// they reach the wire.
const MaxModSeq uint64 = 1<<63 - 1

// Capability gating for this group uses the package-wide sentinel
// [ErrCapabilityNotAdvertised] and its [capabilityError] wrapper rather than a
// second sentinel of its own: one way for a caller to ask "can this server do
// it at all?" is worth more than per-extension precision.

// condStoreAvailable reports whether conditional-store commands may be sent.
//
// RFC 7162 section 3.2.3: "the presence of the QRESYNC capability implies
// support for the CONDSTORE IMAP extension even if the CONDSTORE capability
// isn't advertised", so QRESYNC alone is enough.
func (c *Client) condStoreAvailable() bool {
	return c.Supports("CONDSTORE") || c.Supports("QRESYNC")
}

// CondStoreEnabled reports whether this session has become CONDSTORE-aware.
//
// RFC 7162 section 3.1 lists the "CONDSTORE enabling commands": ENABLE
// CONDSTORE (or ENABLE QRESYNC), SELECT/EXAMINE with the CONDSTORE parameter,
// STATUS (HIGHESTMODSEQ), a FETCH or SEARCH naming the MODSEQ data item, a
// FETCH with CHANGEDSINCE, and a STORE with UNCHANGEDSINCE. Enabling is
// therefore not only an ENABLE thing; this client records the implicit paths it
// takes as well, so the flag reflects the session state the server is in.
//
// Once enabled, the server includes MODSEQ in every subsequent untagged FETCH
// response for the rest of the connection.
func (c *Client) CondStoreEnabled() bool { return c.hasEnabled("CONDSTORE") }

// markCondStoreEnabled records that a CONDSTORE enabling command succeeded.
//
// This mutates connection state rather than a package-level side table because
// that is exactly what RFC 7162 section 3.1 describes: the server's behaviour
// changes for the remainder of the connection, so the fact belongs to the
// connection.
func (c *Client) markCondStoreEnabled() {
	c.mu.Lock()
	c.enabled["CONDSTORE"] = struct{}{}
	c.mu.Unlock()
}

// QResyncEnabled reports whether ENABLE QRESYNC has succeeded on this session.
//
// RFC 7162 section 3.2.3 requires a client using QRESYNC to issue ENABLE
// QRESYNC once authenticated, and requires the server to answer BAD to the
// QRESYNC select parameter or the VANISHED FETCH modifier if it has not. Note
// that this is a property of a successful ENABLE, not of the advertised
// capability, and that ENABLE is only valid in the authenticated state, so the
// order is: authenticate, ENABLE QRESYNC, then SELECT.
//
// Enabling QRESYNC also switches the server from EXPUNGE to VANISHED responses
// for every mailbox that is not NOMODSEQ, for the rest of the connection.
func (c *Client) QResyncEnabled() bool { return c.hasEnabled("QRESYNC") }

// validModSeq reports whether v is a legal mod-sequence-value: RFC 7162
// section 7 restricts it to 1..2^63-1. Zero is legal only for
// mod-sequence-valzer, which UNCHANGEDSINCE and the MODSEQ search key use.
func validModSeq(v uint64) bool { return v >= 1 && v <= MaxModSeq }

// validModSeqZero reports whether v is a legal mod-sequence-valzer.
func validModSeqZero(v uint64) bool { return v <= MaxModSeq }

// writeModSeq encodes a mod-sequence. The value is carried as uint64 all the
// way to this point and only narrowed here, after the range check, so no part
// of the pipeline can truncate it.
func writeModSeq(enc *imapwire.Encoder, v uint64) {
	if v > MaxModSeq {
		// Unreachable through the exported API: every caller validates first.
		// Emitting a negative number64 would be far worse than an encoder error.
		enc.Atom("")
		return
	}
	enc.Number64(int64(v))
}

// numSetSyntax reports whether s is a syntactically valid sequence-set:
// comma-separated ranges, each one or two bounds joined by ":", each bound
// either "*" or an nz-number that fits in 32 bits.
//
// RFC 3501 section 9: seq-number = nz-number / "*". Zero is never a message
// number, which is why it is rejected here rather than passed through.
func numSetSyntax(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return false
		}
		bounds := strings.Split(part, ":")
		if len(bounds) > 2 {
			return false
		}
		for _, bound := range bounds {
			if bound == "*" {
				continue
			}
			if bound == "" {
				return false
			}
			for i := 0; i < len(bound); i++ {
				if bound[i] < '0' || bound[i] > '9' {
					return false
				}
			}
			n, err := strconv.ParseUint(bound, 10, 32)
			if err != nil || n == 0 {
				return false
			}
		}
	}
	return true
}

// writeNumSet encodes a sequence-set.
//
// It exists because [imapwire.Encoder.Atom] rejects "*", which is an IMAP
// list-wildcard and therefore not an ATOM-CHAR, while "1:*" is both legal and
// by far the most common set a client sends. The set is validated by
// [numSetSyntax] before any byte is written, so this stays as strict as Atom
// about what may reach the wire.
func writeNumSet(enc *imapwire.Encoder, set string) {
	if !numSetSyntax(set) {
		enc.Atom(set) // Reports the encoder error in the usual way.
		return
	}
	run := 0
	for i := 0; i < len(set); i++ {
		if set[i] != '*' {
			run++
			continue
		}
		if run > 0 {
			enc.Atom(set[i-run : i])
			run = 0
		}
		enc.Special('*')
	}
	if run > 0 {
		enc.Atom(set[len(set)-run:])
	}
}

// VanishedData is one VANISHED response. It is an alias for
// [imap.VanishedData], which both protocol directions share.
type VanishedData = imap.VanishedData

// readVanished parses an untagged VANISHED response. The decoder is positioned
// immediately after the "VANISHED" atom.
//
// Grammar, RFC 7162 section 7:
//
//	expunged-resp = "VANISHED" [SP "(EARLIER)"] SP known-uids
//	known-uids    = sequence-set ;; "*" is not allowed
func readVanished(dec *imapwire.Decoder) (VanishedData, error) {
	var data VanishedData
	if !dec.ExpectSP() {
		return data, dec.Err()
	}
	if dec.PeekSpecial('(') {
		if !dec.ExpectSpecial('(') {
			return data, dec.Err()
		}
		var word string
		if !dec.ExpectAtom(&word) || !dec.ExpectSpecial(')') {
			return data, dec.Err()
		}
		if !strings.EqualFold(word, "EARLIER") {
			return data, fmt.Errorf("unknown VANISHED modifier %q", word)
		}
		data.Earlier = true
		if !dec.ExpectSP() {
			return data, dec.Err()
		}
	}
	var raw string
	if !dec.ExpectAtom(&raw) {
		return data, dec.Err()
	}
	if !dec.ExpectCRLF() {
		return data, dec.Err()
	}
	uids, err := imap.ParseUIDSet(raw)
	if err != nil {
		return data, fmt.Errorf("invalid VANISHED UID set %q: %w", raw, err)
	}
	if uids.Dynamic() {
		// RFC 7162 section 7: known-uids is a sequence-set in which "*" is not
		// allowed. A "*" here would leave the caller unable to tell which UIDs
		// are gone, which is silent cache corruption rather than a cosmetic
		// grammar violation.
		return data, fmt.Errorf("VANISHED UID set %q must not contain \"*\"", raw)
	}
	data.UIDs = uids
	return data, nil
}

// parseObjectID extracts an object identifier from the parenthesised form used
// by OBJECTID, RFC 8474 section 7:
//
//	objectid = 1*255(ALPHA / DIGIT / "_" / "-")
//
// It accepts "(id)" and also a bare "id", because the parentheses belong to the
// enclosing production rather than to the identifier itself.
func parseObjectID(args string) (string, error) {
	id := strings.TrimSpace(args)
	if strings.HasPrefix(id, "(") {
		if !strings.HasSuffix(id, ")") {
			return "", fmt.Errorf("malformed object identifier %q", args)
		}
		id = id[1 : len(id)-1]
	}
	if id == "" || len(id) > 255 {
		return "", fmt.Errorf("object identifier %q is not 1-255 characters", args)
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_', b == '-':
		default:
			return "", fmt.Errorf("object identifier %q contains an illegal character", args)
		}
	}
	return id, nil
}

// readSyncFetch parses an untagged FETCH response into memory.
//
// It is deliberately not [readFetchResponse]: that one streams body sections
// and blocks until the caller has drained them, which is right for a FETCH the
// caller is iterating and wrong for the flag updates a SELECT collects into a
// slice, where nothing would ever drain the literal. Every value here is either
// modelled or captured with a bounded in-memory copy, so an unexpected literal
// cannot wedge the reader goroutine.
//
// Items this function does not model are preserved as [imap.FetchDataRaw] under
// the exact key the server sent, rather than dropped.
func readSyncFetch(resp *untaggedResponse) (*imap.FetchMessageData, error) {
	dec := resp.dec
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	data := &imap.FetchMessageData{SeqNum: imap.SeqNum(resp.number), Items: make(map[imap.FetchDataKey][]imap.FetchData)}
	err := dec.ExpectList(func() error {
		var key string
		if !dec.ExpectFetchItemName(&key) {
			return dec.Err()
		}
		if dec.PeekSpecial('[') {
			var section imapwire.BodySection
			if !dec.ExpectBodySection(&section) || !dec.ExpectSP() {
				return dec.Err()
			}
			key = formatSectionKey(key, &section)
			return captureSyncFetchValue(dec, data, key)
		}
		if !dec.ExpectSP() {
			return dec.Err()
		}
		var value imap.FetchData
		switch strings.ToUpper(key) {
		case "UID":
			var n uint32
			if !dec.ExpectUniqueID(&n) {
				return dec.Err()
			}
			value = imap.FetchDataUID(n)
		case "MODSEQ":
			// RFC 7162 section 7: fetch-mod-resp = "MODSEQ" SP "("
			// permsg-modsequence ")". The value is a 63-bit unsigned integer
			// and is widened to uint64 here without ever passing through a
			// narrower type.
			var n int64
			if err := dec.ExpectList(func() error {
				if !dec.ExpectNumber64(&n) {
					return dec.Err()
				}
				return nil
			}); err != nil {
				return err
			}
			value = imap.FetchDataModSeq(uint64(n))
		case "FLAGS":
			var flags []string
			if err := dec.ExpectFlagList(&flags); err != nil {
				return err
			}
			value = imap.FetchDataFlags(flagsFromRaw(flags))
		default:
			return captureSyncFetchValue(dec, data, key)
		}
		data.Items[imap.FetchDataKey(key)] = append(data.Items[imap.FetchDataKey(key)], value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return data, nil
}

func captureSyncFetchValue(dec *imapwire.Decoder, data *imap.FetchMessageData, key string) error {
	var raw []byte
	if err := dec.CaptureValue(&raw); err != nil {
		if dec.Err() != nil {
			return err
		}
		// The value was larger than the in-memory limit. It has still been
		// consumed, so the stream stays aligned; the empty reader records that
		// the value existed and could not be kept.
		raw = nil
	}
	data.Items[imap.FetchDataKey(key)] = append(data.Items[imap.FetchDataKey(key)], &imap.FetchDataRaw{Reader: bytes.NewReader(raw)})
	return nil
}
