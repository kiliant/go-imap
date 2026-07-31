package imapnum

import (
	"fmt"
	"iter"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Num is the constraint on the number types a [Set] may hold. It is satisfied
// by uint32 and by any named type whose underlying type is uint32, which is how
// callers instantiate a Set with their own sequence-number or UID type without
// this package having to know about it.
type Num interface {
	~uint32
}

// Wildcard is the internal encoding of the IMAP "*" token, which denotes the
// largest number in use.
//
// Zero is a safe sentinel because seq-number is an nz-number production
// (RFC 3501 section 9): zero is never a valid message number.
const Wildcard = 0

// Range is an inclusive range of numbers, the seq-range production of RFC 3501
// section 9. A zero Stop means "*"; a zero Start with a zero Stop is the bare
// "*"; a zero Start with a non-zero Stop is the "*:n" form, which RFC 3501
// defines as identical to "n:*".
type Range[N Num] struct {
	Start N
	Stop  N
}

// Set is a set of numbers, the sequence-set production of RFC 3501 section 9.
// A nil Set is a valid empty set. Ranges need not be sorted, disjoint or
// normalised; [Set.Normalized] produces the canonical form.
type Set[N Num] []Range[N]

// SetNum returns a Set containing exactly nums. Zero values are ignored.
func SetNum[N Num](nums ...N) Set[N] {
	var s Set[N]
	s.AddNum(nums...)
	return s
}

// SetRange returns a Set containing the single range start:stop.
func SetRange[N Num](start, stop N) Set[N] {
	var s Set[N]
	s.AddRange(start, stop)
	return s
}

// Bounds returns the range as a closed interval over uint32, with "*" mapped to
// the largest representable value, and with a reversed range put in order.
func (r Range[N]) Bounds() (lo, hi uint32) {
	lo, hi = uint32(r.Start), uint32(r.Stop)
	if lo == Wildcard {
		lo = math.MaxUint32
	}
	if hi == Wildcard {
		hi = math.MaxUint32
	}
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo, hi
}

// RangeFromBounds is the inverse of [Range.Bounds].
func RangeFromBounds[N Num](lo, hi uint32, dynamic bool) Range[N] {
	if dynamic && hi == math.MaxUint32 {
		if lo == math.MaxUint32 {
			return Range[N]{}
		}
		return Range[N]{Start: N(lo), Stop: Wildcard}
	}
	return Range[N]{Start: N(lo), Stop: N(hi)}
}

// IsDynamic reports whether the range mentions "*".
func (r Range[N]) IsDynamic() bool { return r.Start == Wildcard || r.Stop == Wildcard }

// String formats the range as "5", "1:5" or "1:*".
func (r Range[N]) String() string {
	start, stop := formatNum(r.Start), formatNum(r.Stop)
	if r.Start == r.Stop {
		return start
	}
	return start + ":" + stop
}

// Dynamic reports whether any range in the set mentions "*".
func (s Set[N]) Dynamic() bool {
	for _, r := range s {
		if r.IsDynamic() {
			return true
		}
	}
	return false
}

// IsEmpty reports whether the set holds no ranges.
func (s Set[N]) IsEmpty() bool { return len(s) == 0 }

// Contains reports whether n is a member of the set. "*" is treated as the
// largest representable number, so for a dynamic set the result is an upper
// bound on the true membership. Zero is never a member.
func (s Set[N]) Contains(n N) bool {
	if n == 0 {
		return false
	}
	v := uint32(n)
	for _, r := range s {
		lo, hi := r.Bounds()
		if lo <= v && v <= hi {
			return true
		}
	}
	return false
}

// Normalized returns an equivalent set in canonical form: ranges sorted
// ascending, overlapping and adjacent ranges coalesced, "*:n" rewritten to
// "n:*", and reversed ranges put in order. The receiver is not modified.
func (s Set[N]) Normalized() Set[N] {
	if len(s) == 0 {
		return nil
	}
	type iv struct {
		lo, hi  uint32
		dynamic bool
	}
	ivs := make([]iv, 0, len(s))
	for _, r := range s {
		lo, hi := r.Bounds()
		ivs = append(ivs, iv{lo: lo, hi: hi, dynamic: r.IsDynamic()})
	}
	slices.SortFunc(ivs, func(a, b iv) int {
		if a.lo != b.lo {
			return cmpU32(a.lo, b.lo)
		}
		return cmpU32(a.hi, b.hi)
	})
	out := make(Set[N], 0, len(ivs))
	cur := ivs[0]
	for _, next := range ivs[1:] {
		if cur.hi == math.MaxUint32 || next.lo <= cur.hi+1 {
			if next.hi > cur.hi {
				cur.hi = next.hi
			}
			cur.dynamic = cur.dynamic || next.dynamic
			continue
		}
		out = append(out, RangeFromBounds[N](cur.lo, cur.hi, cur.dynamic))
		cur = next
	}
	out = append(out, RangeFromBounds[N](cur.lo, cur.hi, cur.dynamic))
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

// AddNum adds nums to the set and renormalises it. Zero values are ignored.
func (s *Set[N]) AddNum(nums ...N) {
	for _, n := range nums {
		if n == 0 {
			continue
		}
		*s = append(*s, Range[N]{Start: n, Stop: n})
	}
	*s = s.Normalized()
}

// AddRange adds the inclusive range start:stop and renormalises the set. A zero
// start or stop means "*".
func (s *Set[N]) AddRange(start, stop N) {
	*s = append(*s, Range[N]{Start: start, Stop: stop})
	*s = s.Normalized()
}

// AddSet adds every member of other and renormalises the set.
func (s *Set[N]) AddSet(other Set[N]) {
	*s = append(*s, other...)
	*s = s.Normalized()
}

// Equal reports whether s and other contain the same numbers, comparing
// normalised forms.
func (s Set[N]) Equal(other Set[N]) bool {
	return slices.Equal(s.Normalized(), other.Normalized())
}

// All iterates over the members of the set in ascending order. Dynamic ranges
// cannot be enumerated and are skipped; test [Set.Dynamic] first when that
// matters.
func (s Set[N]) All() iter.Seq[N] {
	return func(yield func(N) bool) {
		for _, r := range s.Normalized() {
			if r.IsDynamic() {
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

// Nums returns the members of the set in ascending order and reports whether
// the set could be enumerated exactly; it returns false for a dynamic set.
//
// A set such as "1:4000000000" is enumerable but enormous. Prefer [Set.All].
func (s Set[N]) Nums() ([]N, bool) {
	if s.Dynamic() {
		return nil, false
	}
	return slices.Collect(s.All()), true
}

// String formats the set using the IMAP sequence-set syntax, for example
// "1:5,7,9:*". An empty set formats as "".
func (s Set[N]) String() string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(r.String())
	}
	return b.String()
}

func formatNum[N Num](n N) string {
	if n == Wildcard {
		return "*"
	}
	return strconv.FormatUint(uint64(n), 10)
}

// Parse parses the IMAP sequence-set syntax, for example "1:5,7,9:*".
// RFC 3501 section 9.
//
// The result is not normalised. Errors are plain errors: this package must not
// depend on the root imap package, so wrapping them in an *imap.Error is the
// caller's job.
func Parse[N Num](s string) (Set[N], error) {
	if s == "" {
		return nil, fmt.Errorf("imapnum: empty sequence set")
	}
	parts := strings.Split(s, ",")
	out := make(Set[N], 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("imapnum: sequence set %q: empty element", s)
		}
		r, err := parseRange[N](s, part)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func parseRange[N Num](whole, part string) (Range[N], error) {
	var r Range[N]
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

func parseNum[N Num](whole, tok string) (N, error) {
	if tok == "*" {
		return Wildcard, nil
	}
	if tok == "" {
		return 0, fmt.Errorf("imapnum: sequence set %q: empty number", whole)
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return 0, fmt.Errorf("imapnum: sequence set %q: invalid number %q", whole, tok)
		}
	}
	v, err := strconv.ParseUint(tok, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("imapnum: sequence set %q: number %q out of range", whole, tok)
	}
	if v == 0 {
		return 0, fmt.Errorf("imapnum: sequence set %q: zero is not a valid message number", whole)
	}
	return N(v), nil
}
