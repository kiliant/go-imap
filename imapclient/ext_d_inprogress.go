package imapclient

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// InProgressData is one INPROGRESS progress notification. RFC 9585.
//
// Construct with keyed fields only; fields may be added in a future release.
type InProgressData struct {
	// Tag is the originating command tag, or "" when the server sent NIL
	// (or omitted the detail list entirely).
	Tag string

	// Progress is the number of items processed so far. Valid only when
	// HasProgress is set. RFC 9585 requires a non-negative value.
	Progress    uint32
	HasProgress bool

	// Goal is the expected total. Valid only when HasGoal is set. When set it
	// must be strictly greater than Progress; malformed notifications are
	// discarded by [ParseInProgressArgs].
	Goal    uint32
	HasGoal bool

	// Text is the human-readable text that accompanied the untagged OK, if
	// known. It is empty when only the response-code arguments were parsed.
	Text string

	_ struct{}
}

// inProgressOptions is reserved for command-scoped progress callbacks once
// core command options grow an InProgress field (see the T11 escalation).
// Until then callers parse [imap.CodeInProgress] arguments with
// [ParseInProgressArgs].
//
// Construct with keyed fields only; fields may be added in a future release.
type inProgressOptions struct {
	// Handler receives progress notifications claimed by a collector that
	// wraps [claimInProgress]. A nil Handler ignores notifications.
	// Called on the reader goroutine; must not block.
	Handler func(*InProgressData)
	_       struct{}
}

// SupportsInProgress reports whether the server advertises INPROGRESS.
// INPROGRESS, RFC 9585.
func (c *Client) SupportsInProgress() bool {
	return c.Supports("INPROGRESS")
}

// ParseInProgressArgs parses the arguments of an INPROGRESS response code.
// RFC 9585 section 5.
//
// Accepts the bare form (no arguments — all fields NIL), and the list form
// ("tag-or-NIL" progress-or-NIL goal-or-NIL). Notifications that violate the
// RFC 9585 security constraints (GOAL = 0, PROGRESS ≥ GOAL when GOAL is set)
// return an error so callers can disregard them.
func ParseInProgressArgs(args string) (*InProgressData, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return &InProgressData{}, nil
	}
	dec := imapwire.NewDecoderString(args+"\r\n", nil)
	if !dec.ExpectSpecial('(') {
		return nil, fmt.Errorf("INPROGRESS arguments must be a parenthesised list or empty: %q", args)
	}
	data := &InProgressData{}
	var tag string
	var tagNil bool
	if !dec.ExpectNString(&tag, &tagNil) || !dec.ExpectSP() {
		return nil, fmt.Errorf("invalid INPROGRESS tag in %q", args)
	}
	if !tagNil {
		data.Tag = tag
	}
	var progressAtom string
	if !dec.ExpectAtom(&progressAtom) || !dec.ExpectSP() {
		return nil, fmt.Errorf("invalid INPROGRESS progress in %q", args)
	}
	var goalAtom string
	if !dec.ExpectAtom(&goalAtom) {
		return nil, fmt.Errorf("invalid INPROGRESS goal in %q", args)
	}
	if !dec.ExpectSpecial(')') {
		return nil, fmt.Errorf("INPROGRESS list not closed in %q", args)
	}
	if !strings.EqualFold(progressAtom, "NIL") {
		n, err := strconv.ParseUint(progressAtom, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid INPROGRESS progress %q", progressAtom)
		}
		data.Progress = uint32(n)
		data.HasProgress = true
	}
	if !strings.EqualFold(goalAtom, "NIL") {
		n, err := strconv.ParseUint(goalAtom, 10, 32)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("invalid INPROGRESS goal %q", goalAtom)
		}
		data.Goal = uint32(n)
		data.HasGoal = true
	}
	if data.HasProgress && !data.HasGoal && strings.EqualFold(goalAtom, "NIL") {
		// Counting form: progress known, goal unknown — fine.
	}
	if !data.HasProgress && data.HasGoal {
		return nil, fmt.Errorf("INPROGRESS goal without progress in %q", args)
	}
	if data.HasProgress && data.HasGoal && data.Progress >= data.Goal {
		// RFC 9585 §6: disregard PROGRESS ≥ GOAL.
		return nil, fmt.Errorf("INPROGRESS progress %d not less than goal %d", data.Progress, data.Goal)
	}
	return data, nil
}

// claimInProgress recognises an untagged OK [INPROGRESS ...] and invokes
// handler when non-nil. It is intended for collectors that wrap another
// collector; connection-level delivery requires a Client.Options / conn.go
// hook owned by T03 (escalated in .state/progress/T11.md).
func claimInProgress(resp *untaggedResponse, handler func(*InProgressData)) (bool, error) {
	if resp == nil || resp.cond == nil || resp.name != "OK" {
		return false, nil
	}
	if !strings.EqualFold(resp.cond.Text.Code, string(imap.CodeInProgress)) {
		return false, nil
	}
	data, err := ParseInProgressArgs(resp.cond.Text.Args)
	if err != nil {
		// Malformed or security-rejected notifications are disregarded, not
		// fatal to the command — RFC 9585 section 6.
		return true, nil
	}
	data.Text = resp.cond.Text.Text
	if handler != nil {
		handler(data)
	}
	return true, nil
}
