package imap

// UIDBatchesData is the correlated UIDBATCHES response (RFC 10022).
// Construct with keyed fields only; fields may be added in a future release.
type UIDBatchesData struct {
	Tag string
	// Batches retains wire order, newest batch first. Endpoints may name
	// nonexistent UIDs and either endpoint order covers the same interval.
	Batches []UIDBatch
	_       struct{}
}

// UIDBatch is one batch. Its range endpoints are non-zero, never "*".
// Construct with keyed fields only; fields may be added in a future release.
type UIDBatch struct {
	Range UIDRange
	_     struct{}
}

// UIDBatchIndexRange names inclusive, one-based batch indices, newest first.
// Both endpoints zero omit the range; otherwise both must be non-zero and
// First must not exceed Last.
// Construct with keyed fields only; fields may be added in a future release.
type UIDBatchIndexRange struct {
	First, Last uint32
	_           struct{}
}
