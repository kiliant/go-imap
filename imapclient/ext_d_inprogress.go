package imapclient

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// InProgressData is one INPROGRESS progress notification. It is an alias for
// [imap.InProgressData], which both protocol directions share.
type InProgressData = imap.InProgressData

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
