package imap

import (
	"iter"
	"math"
	"slices"
	"strconv"
	"strings"
)

// NumKind distinguishes message sequence numbers from unique identifiers.
//
// IMAP addresses messages two ways: by sequence number, which is a position in
// the mailbox and changes as messages are expunged, and by UID, which is stable
// for the lifetime of a UIDVALIDITY value (RFC 3501 section 2.3.1.1). Passing
// one where the other is expected silently addresses the wrong messages, which
// is the classic IMAP client data-corruption bug.
//
// The distinction is therefore carried in the type system: [SeqNum] and [UID]
// are distinct types and [SeqSet] and [UIDSet] are distinct types, so the
// mistake does not compile. NumKind exists only for the places where the kind
// is genuinely data — for instance an ESEARCH response that reports which kind
// it used (RFC 4731) — and never as a parameter that selects behaviour.
type NumKind int

const (
	// NumKindUnknown is the zero value: no kind has been determined.
	NumKindUnknown NumKind = iota
	// NumKindSeq is a message sequence number. RFC 3501 section 2.3.1.2.
	NumKindSeq
	// NumKindUID is a unique identifier. RFC 3501 section 2.3.1.1.
	NumKindUID
)

// String returns "seq", "uid" or "unknown".
func (k NumKind) String() string {
	switch k {
	case NumKindSeq:
		return "seq"
	case NumKindUID:
		return "uid"
	default:
		return "unknown"
	}
}

// SeqNum is a message sequence number: the 1-based position of a message in the
// currently selected mailbox. RFC 3501 section 2.3.1.2.
//
// Sequence numbers are renumbered by EXPUNGE. Use [UID] for anything that must
// survive a change to the mailbox.
type SeqNum uint32

// UID is a message unique identifier. RFC 3501 section 2.3.1.1.
//
// A UID is unique and ascending within a mailbox for as long as the mailbox's
// UIDVALIDITY value is unchanged.
type UID uint32

// numType is the closed set of message-number types. It is unexported, and its
// type set is a union of named types rather than an approximation term, so
// external code cannot instantiate [NumSet] or [NumRange] with a type of its
// own. Adding a member is an additive change.
type numType interface {
	SeqNum | UID
}

// wildcard is the internal encoding of the IMAP "*" token, which denotes the
// largest number in use in the mailbox (RFC 3501 section 9, seq-number).
//
// Zero is a safe sentinel: sequence numbers and UIDs are nz-number productions,
// so zero is never a valid message number.
const wildcard = 0

// NumRange is an inclusive range of message numbers, the seq-range production
// of RFC 3501 section 9.
//
// A zero Stop means "*", the largest number in use. A zero Start with a zero
// Stop is the bare "*". A zero Start with a non-zero Stop is the "*:n" form,
// which RFC 3501 defines as identical to "n:*"; [NumSet.Normalized] rewrites it
// to that form.
//
// NumRange is the one exported struct in this package that does NOT carry the
// `_ struct{}` unkeyed-literal guard, so NumRange[SeqNum]{1, 5} is legal and
// idiomatic. The exception is deliberate and is recorded in
// docs/API-STABILITY.md rule 7: a range is exactly a start and a stop, and a
// third field would change what "range" means rather than extend it, so the
// growth this package guards against cannot happen here.
type NumRange[N numType] struct {
	Start N
	Stop  N
}

// NumSet is a set of message numbers, the sequence-set production of RFC 3501
// section 9.
//
// It is a slice so that the common cases are cheap and a nil value is a valid
// empty set. Ranges are not required to be sorted, disjoint or normalised;
// [NumSet.Normalized] produces the canonical form and the mutating methods
// maintain it.
//
// Use the [SeqSet] and [UIDSet] aliases rather than naming NumSet directly.
type NumSet[N numType] []NumRange[N]

// Aliases naming the two instantiations of [NumSet] and [NumRange] that IMAP
// actually uses. They are distinct types: a UIDSet cannot be passed where a
// SeqSet is expected.
type (
	// SeqSet is a set of message sequence numbers.
	SeqSet = NumSet[SeqNum]
	// UIDSet is a set of message unique identifiers.
	UIDSet = NumSet[UID]
	// SeqRange is an inclusive range of message sequence numbers.
	SeqRange = NumRange[SeqNum]
	// UIDRange is an inclusive range of message unique identifiers.
	UIDRange = NumRange[UID]
)

// SeqSetNum returns a [SeqSet] containing exactly nums.
func SeqSetNum(nums ...SeqNum) SeqSet {
	var s SeqSet
	s.AddNum(nums...)
	return s
}

// SeqSetRange returns a [SeqSet] containing the single range start:stop. Pass
// zero for stop to mean "*".
func SeqSetRange(start, stop SeqNum) SeqSet {
	var s SeqSet
	s.AddRange(start, stop)
	return s
}

// UIDSetNum returns a [UIDSet] containing exactly uids.
func UIDSetNum(uids ...UID) UIDSet {
	var s UIDSet
	s.AddNum(uids...)
	return s
}

// UIDSetRange returns a [UIDSet] containing the single range start:stop. Pass
// zero for stop to mean "*".
func UIDSetRange(start, stop UID) UIDSet {
	var s UIDSet
	s.AddRange(start, stop)
	return s
}

// Kind reports whether the set holds sequence numbers or UIDs.
func (s NumSet[N]) Kind() NumKind {
	var zero N
	switch any(zero).(type) {
	case SeqNum:
		return NumKindSeq
	case UID:
		return NumKindUID
	default:
		return NumKindUnknown
	}
}

// Dynamic reports whether the set contains "*", and therefore whether its
// membership depends on the current state of the mailbox.
func (s NumSet[N]) Dynamic() bool {
	for _, r := range s {
		if r.Start == wildcard || r.Stop == wildcard {
			return true
		}
	}
	return false
}

// IsEmpty reports whether the set contains no numbers at all.
func (s NumSet[N]) IsEmpty() bool { return len(s) == 0 }

// bounds returns the range as a closed interval over uint32, with "*" mapped to
// the largest representable value.
func (r NumRange[N]) bounds() (lo, hi uint32) {
	lo, hi = uint32(r.Start), uint32(r.Stop)
	if lo == wildcard {
		lo = math.MaxUint32
	}
	if hi == wildcard {
		hi = math.MaxUint32
	}
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo, hi
}

// rangeFromBounds is the inverse of [NumRange.bounds].
func rangeFromBounds[N numType](lo, hi uint32, dynamic bool) NumRange[N] {
	if dynamic && hi == math.MaxUint32 {
		if lo == math.MaxUint32 {
			return NumRange[N]{} // the bare "*"
		}
		return NumRange[N]{Start: N(lo), Stop: wildcard}
	}
	return NumRange[N]{Start: N(lo), Stop: N(hi)}
}

// Contains reports whether n is a member of the set.
//
// "*" is treated as the largest representable number, so for a dynamic set the
// result is an upper bound on the true membership: "5:*" reports true for any
// n >= 5 even if the mailbox holds fewer messages. Test [NumSet.Dynamic] first
// when that matters. Zero is never a member.
func (s NumSet[N]) Contains(n N) bool {
	if n == 0 {
		return false
	}
	v := uint32(n)
	for _, r := range s {
		lo, hi := r.bounds()
		if lo <= v && v <= hi {
			return true
		}
	}
	return false
}

// Normalized returns an equivalent set in canonical form: ranges sorted
// ascending, overlapping and adjacent ranges coalesced, "*:n" rewritten to
// "n:*", and reversed ranges put in order. The receiver is not modified.
//
// Because "*" is compared as the largest representable number, a range ending
// at 4294967295 coalesces with a range ending in "*".
func (s NumSet[N]) Normalized() NumSet[N] {
	if len(s) == 0 {
		return nil
	}
	type iv struct {
		lo, hi  uint32
		dynamic bool
	}
	ivs := make([]iv, 0, len(s))
	for _, r := range s {
		lo, hi := r.bounds()
		ivs = append(ivs, iv{lo: lo, hi: hi, dynamic: r.Start == wildcard || r.Stop == wildcard})
	}
	slices.SortFunc(ivs, func(a, b iv) int {
		if c := cmpU32(a.lo, b.lo); c != 0 {
			return c
		}
		return cmpU32(a.hi, b.hi)
	})
	out := make(NumSet[N], 0, len(ivs))
	cur := ivs[0]
	for _, next := range ivs[1:] {
		// Adjacent or overlapping: merge. cur.hi+1 would overflow when
		// cur.hi is the maximum, but then every following range is
		// already covered.
		if cur.hi == math.MaxUint32 || next.lo <= cur.hi+1 {
			if next.hi > cur.hi {
				cur.hi = next.hi
			}
			cur.dynamic = cur.dynamic || next.dynamic
			continue
		}
		out = append(out, rangeFromBounds[N](cur.lo, cur.hi, cur.dynamic))
		cur = next
	}
	out = append(out, rangeFromBounds[N](cur.lo, cur.hi, cur.dynamic))
	return out
}

func cmpU32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// AddNum adds nums to the set and renormalises it. Zero values are ignored;
// use [NumSet.AddRange] with a zero Stop to add "*".
func (s *NumSet[N]) AddNum(nums ...N) {
	for _, n := range nums {
		if n == 0 {
			continue
		}
		*s = append(*s, NumRange[N]{Start: n, Stop: n})
	}
	*s = s.Normalized()
}

// AddRange adds the inclusive range start:stop to the set and renormalises it.
// A zero start or stop means "*".
func (s *NumSet[N]) AddRange(start, stop N) {
	*s = append(*s, NumRange[N]{Start: start, Stop: stop})
	*s = s.Normalized()
}

// AddSet adds every member of other to the set and renormalises it.
func (s *NumSet[N]) AddSet(other NumSet[N]) {
	*s = append(*s, other...)
	*s = s.Normalized()
}

// Equal reports whether s and other contain the same numbers. Both are
// normalised first, so "1,2,3" equals "1:3".
func (s NumSet[N]) Equal(other NumSet[N]) bool {
	return slices.Equal(s.Normalized(), other.Normalized())
}

// All iterates over the members of the set in ascending order.
//
// A dynamic range cannot be enumerated, because only the server knows how many
// messages the mailbox holds: All yields the members of the static ranges and
// skips the dynamic ones. Test [NumSet.Dynamic] first when that distinction
// matters.
func (s NumSet[N]) All() iter.Seq[N] {
	return func(yield func(N) bool) {
		for _, r := range s.Normalized() {
			if r.Start == wildcard || r.Stop == wildcard {
				continue
			}
			for n := r.Start; ; n++ {
				if !yield(n) {
					return
				}
				if n == r.Stop {
					break
				}
			}
		}
	}
}

// Nums returns the members of the set in ascending order, and reports whether
// the set could be enumerated exactly. It returns false for a dynamic set,
// because "*" cannot be resolved without the server.
//
// A set such as "1:4000000000" is enumerable but enormous. Prefer
// [NumSet.All], which does not allocate the whole slice.
func (s NumSet[N]) Nums() ([]N, bool) {
	if s.Dynamic() {
		return nil, false
	}
	return slices.Collect(s.All()), true
}

// String formats the set using the IMAP sequence-set syntax, for example
// "1:5,7,9:*". An empty set formats as "".
func (s NumSet[N]) String() string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(r.String())
	}
	return b.String()
}

// String formats the range using the IMAP seq-range syntax: "5", "1:5" or
// "1:*".
func (r NumRange[N]) String() string {
	start, stop := formatNum(r.Start), formatNum(r.Stop)
	if r.Start == r.Stop {
		return start
	}
	return start + ":" + stop
}

func formatNum[N numType](n N) string {
	if n == wildcard {
		return "*"
	}
	return strconv.FormatUint(uint64(n), 10)
}

// ParseSeqSet parses the IMAP sequence-set syntax into a [SeqSet], for example
// "1:5,7,9:*". RFC 3501 section 9.
//
// It returns an [Error] with Type [ErrorTypeProtocol] if s is not a valid
// sequence set. The result is not normalised; call [NumSet.Normalized] if a
// canonical form is wanted.
func ParseSeqSet(s string) (SeqSet, error) { return parseNumSet[SeqNum](s) }

// ParseUIDSet parses the IMAP sequence-set syntax into a [UIDSet]. RFC 3501
// section 9. See [ParseSeqSet].
func ParseUIDSet(s string) (UIDSet, error) { return parseNumSet[UID](s) }

func parseNumSet[N numType](s string) (NumSet[N], error) {
	if s == "" {
		return nil, newProtocolError("empty sequence set")
	}
	parts := strings.Split(s, ",")
	out := make(NumSet[N], 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, newProtocolError("sequence set %q: empty element", s)
		}
		r, err := parseNumRange[N](s, part)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func parseNumRange[N numType](whole, part string) (NumRange[N], error) {
	var r NumRange[N]
	start, stop, hasColon := strings.Cut(part, ":")
	n, err := parseNum[N](whole, start)
	if err != nil {
		return r, err
	}
	r.Start = n
	if !hasColon {
		r.Stop = n
		return r, nil
	}
	n, err = parseNum[N](whole, stop)
	if err != nil {
		return r, err
	}
	r.Stop = n
	return r, nil
}

func parseNum[N numType](whole, tok string) (N, error) {
	if tok == "*" {
		return wildcard, nil
	}
	if tok == "" {
		return 0, newProtocolError("sequence set %q: empty number", whole)
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return 0, newProtocolError("sequence set %q: invalid number %q", whole, tok)
		}
	}
	v, err := strconv.ParseUint(tok, 10, 32)
	if err != nil {
		return 0, newProtocolError("sequence set %q: number %q out of range", whole, tok)
	}
	if v == 0 {
		// seq-number is an nz-number: zero is never valid, which is what
		// makes zero usable as the internal encoding of "*".
		return 0, newProtocolError("sequence set %q: zero is not a valid message number", whole)
	}
	return N(v), nil
}
