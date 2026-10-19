package memory

import (
	"bytes"
	"context"
	"io"
	"math"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	servermessage "github.com/kiliant/go-imap/imapserver/message"
)

func (s *session) MultiAppend(ctx context.Context, name string, messages []imapserver.AppendMessage, options *imapserver.MultiAppendOptions) (*imap.MultiAppendData, error) {
	if len(messages) == 0 {
		return nil, noError(imap.CodeCannot, "empty append batch")
	}
	staged := make([]*message, 0, len(messages))
	for _, input := range messages {
		if input.Literal == nil {
			return nil, noError(imap.CodeCannot, "nil message literal")
		}
		raw, err := io.ReadAll(&contextReader{ctx: ctx, reader: input.Literal})
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 || input.HasSize && (input.Size < 0 || int64(len(raw)) != input.Size) {
			return nil, noError(imap.CodeCannot, "empty or truncated message literal")
		}
		analysis, err := servermessage.Analyze(ctx, bytes.NewReader(raw), int64(len(raw)), nil)
		if err != nil {
			return nil, err
		}
		date := input.InternalDate
		if date.IsZero() {
			date = time.Now()
		}
		staged = append(staged, &message{recent: true, raw: raw, analysis: analysis, flags: uniqueFlags(input.Flags), internalDate: date, saveDate: time.Now()})
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed {
		return nil, noError(imap.CodeClosed, "session is closed")
	}
	m := s.account.mailboxes[mailboxKey(name)]
	if m == nil {
		return nil, noError(imap.CodeTryCreate, "destination mailbox does not exist")
	}
	if m.uidNext == 0 || uint64(m.uidNext)+uint64(len(staged)) > math.MaxUint32 ||
		m.highestModSeq > math.MaxInt64-uint64(len(staged)) || m.revision == math.MaxUint64 {
		return nil, noError(imap.CodeLimit, "mailbox counter space exhausted")
	}
	uids := make([]imap.UID, len(staged))
	var assigned imap.UIDSet
	flagsChanged := false
	for i, msg := range staged {
		msg.uid = m.uidNext
		m.uidNext++
		msg.modSeq = bumpModSeqLocked(m)
		uids[i] = msg.uid
		assigned.AddNum(msg.uid)
		flagsChanged = ensureMailboxFlagsLocked(m, msg.flags) || flagsChanged
	}
	m.messages = append(m.messages, staged...)
	changes := []imapserver.Update{&imapserver.UpdateAdd{UIDs: uids}}
	if flagsChanged {
		changes = append([]imapserver.Update{&imapserver.UpdateMailboxFlags{Flags: mailboxFlagsLocked(m)}}, changes...)
	}
	var origin imapserver.ChangeToken
	if options != nil {
		origin = options.Origin
	}
	publishLocked(m, advanceLocked(m, origin, changes))
	notifyMailboxLocked(s.account, m)
	return &imap.MultiAppendData{HasUIDs: true, UIDValidity: m.uidValidity, UIDs: assigned}, nil
}

var _ imapserver.MultiAppendSession = (*session)(nil)
