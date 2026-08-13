package memory

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/internal/imapmessage"
)

type message struct {
	uid          imap.UID
	flags        []imap.Flag
	internalDate time.Time
	raw          []byte
	analysis     *imapmessage.Message
	// modSeq is the CONDSTORE modification sequence, bumped on every change to
	// this message's flags. See ext_b.go.
	modSeq uint64
	// saveDate is when the message was placed in this mailbox, which SAVEDATE
	// distinguishes from the internal date the message carries.
	saveDate time.Time
}

type selected struct {
	session  *session
	mailbox  *mailbox
	readOnly bool
	closed   bool
}

func (s *selected) Status(ctx context.Context, _ *imapserver.StatusOptions) (*imap.MailboxStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.session.account.mu.Lock()
	defer s.session.account.mu.Unlock()
	if s.closed {
		return nil, noError(imap.CodeClosed, "mailbox is no longer selected")
	}
	snapshot := snapshotLocked(s.mailbox, s.readOnly)
	return &snapshot.Status, nil
}

func (s *selected) Fetch(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, options *imapserver.FetchOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.session.account.mu.Lock()
	if s.closed {
		s.session.account.mu.Unlock()
		return noError(imap.CodeClosed, "mailbox is no longer selected")
	}
	items := []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags}
	if options != nil && len(options.Items) != 0 {
		items = options.Items
	}
	var results []*imap.FetchMessageData
	var flagUpdates []imapserver.Update
	changedSince := uint64(0)
	if options != nil {
		changedSince = options.ChangedSince
	}
	for i, msg := range s.mailbox.messages {
		if !uids.Contains(msg.uid) {
			continue
		}
		// CONDSTORE's CHANGEDSINCE restricts the result to messages modified
		// after the client's modification sequence. RFC 7162 section 3.1.4.
		if changedSince != 0 && msg.modSeq <= changedSince {
			continue
		}
		data, marksSeen, err := fetchMessageData(msg, imap.SeqNum(i+1), items)
		if err != nil {
			s.session.account.mu.Unlock()
			return err
		}
		if marksSeen && !s.readOnly && !imap.ContainsFlag(msg.flags, imap.FlagSeen) {
			msg.flags = append(msg.flags, imap.FlagSeen)
			msg.modSeq = bumpModSeqLocked(s.mailbox)
			if _, requested := data.Items[imap.FetchDataKey(imap.FetchItemFlags)]; requested {
				data.Items[imap.FetchDataKey(imap.FetchItemFlags)] = []imap.FetchData{imap.FetchDataFlags(cloneFlags(msg.flags))}
			}
			flagUpdates = append(flagUpdates, &imapserver.UpdateFlags{UID: msg.uid, Flags: cloneFlags(msg.flags), ModSeq: msg.modSeq})
		}
		results = append(results, data)
	}
	if len(flagUpdates) != 0 {
		batch := advanceLocked(s.mailbox, 0, flagUpdates)
		publishLocked(s.mailbox, batch)
	}
	s.session.account.mu.Unlock()
	for _, data := range results {
		if err := writer.WriteMessage(ctx, data); err != nil {
			return err
		}
	}
	return nil
}

func (s *selected) Search(ctx context.Context, query *imapserver.SearchQuery, options *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.session.account.mu.Lock()
	if s.closed {
		s.session.account.mu.Unlock()
		return nil, noError(imap.CodeClosed, "mailbox is no longer selected")
	}
	criteria := query.Criteria()
	if criteria == nil {
		criteria = imap.SearchAll
	}
	charset := ""
	if options != nil {
		charset = options.Charset
	}
	var matches []imap.UID
	for i, msg := range s.mailbox.messages {
		metadata := imapmessage.Metadata{
			SeqNum:       imap.SeqNum(i + 1),
			UID:          msg.uid,
			Flags:        cloneFlags(msg.flags),
			InternalDate: msg.internalDate,
			RFC822Size:   int64(len(msg.raw)),
		}
		matched, err := imapmessage.Match(msg.analysis, metadata, criteria, &imapmessage.MatchOptions{Charset: charset})
		if err != nil {
			s.session.account.mu.Unlock()
			return nil, err
		}
		if matched {
			matches = append(matches, msg.uid)
		}
	}
	s.session.account.mu.Unlock()
	return &imapserver.SearchResult{UIDs: matches}, nil
}

func (s *selected) Store(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, store *imapserver.StoreFlags, options *imapserver.StoreOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.readOnly {
		return noError(imap.CodeReadOnly, "mailbox is read-only")
	}
	if store == nil {
		return noError(imap.CodeClientBug, "nil STORE flags")
	}
	s.session.account.mu.Lock()
	if s.closed {
		s.session.account.mu.Unlock()
		return noError(imap.CodeClosed, "mailbox is no longer selected")
	}
	var origin imapserver.ChangeToken
	silent := false
	if options != nil {
		origin, silent = options.Origin, options.Silent
	}
	var changes []imapserver.Update
	var results []*imap.FetchMessageData
	for i, msg := range s.mailbox.messages {
		if !uids.Contains(msg.uid) {
			continue
		}
		flags, err := applyStoreFlags(msg.flags, store)
		if err != nil {
			s.session.account.mu.Unlock()
			return err
		}
		msg.flags = flags
		msg.modSeq = bumpModSeqLocked(s.mailbox)
		changes = append(changes, &imapserver.UpdateFlags{UID: msg.uid, Flags: cloneFlags(flags), ModSeq: msg.modSeq})
		if !silent {
			results = append(results, flagsFetchData(imap.SeqNum(i+1), msg))
		}
	}
	if len(changes) != 0 {
		batch := advanceLocked(s.mailbox, origin, changes)
		publishLocked(s.mailbox, batch)
	}
	s.session.account.mu.Unlock()
	for _, data := range results {
		if err := writer.WriteMessage(ctx, data); err != nil {
			return err
		}
	}
	return nil
}

func (s *selected) Copy(ctx context.Context, uids imap.UIDSet, destination string, options *imapserver.CopyOptions) (*imap.CopyData, error) {
	var origin imapserver.ChangeToken
	if options != nil {
		origin = options.Origin
	}
	return s.copyOrMove(ctx, uids, destination, origin, false)
}

func (s *selected) Move(ctx context.Context, uids imap.UIDSet, destination string, options *imapserver.MoveOptions) (*imap.CopyData, error) {
	if s.readOnly {
		return nil, noError(imap.CodeReadOnly, "mailbox is read-only")
	}
	var origin imapserver.ChangeToken
	if options != nil {
		origin = options.Origin
	}
	return s.copyOrMove(ctx, uids, destination, origin, true)
}

func (s *selected) copyOrMove(ctx context.Context, uids imap.UIDSet, destination string, origin imapserver.ChangeToken, move bool) (*imap.CopyData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.session.account.mu.Lock()
	defer s.session.account.mu.Unlock()
	if s.closed {
		return nil, noError(imap.CodeClosed, "mailbox is no longer selected")
	}
	destinationMailbox := s.session.account.mailboxes[mailboxKey(destination)]
	if destinationMailbox == nil {
		return nil, noError(imap.CodeTryCreate, "destination mailbox does not exist")
	}
	if move && destinationMailbox == s.mailbox {
		return nil, noError(imap.CodeCannot, "cannot move messages to the selected mailbox")
	}
	var sources, destinations []imap.UID
	var additions []imapserver.Update
	var removals []imapserver.Update
	original := append([]*message(nil), s.mailbox.messages...)
	retained := make([]*message, 0, len(original))
	for _, msg := range original {
		if !uids.Contains(msg.uid) {
			if move {
				retained = append(retained, msg)
			}
			continue
		}
		uid := destinationMailbox.uidNext
		destinationMailbox.uidNext++
		clone := *msg
		clone.uid = uid
		clone.flags = cloneFlags(msg.flags)
		destinationMailbox.messages = append(destinationMailbox.messages, &clone)
		sources, destinations = append(sources, msg.uid), append(destinations, uid)
		additions = append(additions, &imapserver.UpdateAdd{UIDs: []imap.UID{uid}})
		if move {
			removals = append(removals, &imapserver.UpdateExpunge{UID: msg.uid})
		}
	}
	if len(additions) != 0 {
		publishLocked(destinationMailbox, advanceLocked(destinationMailbox, origin, additions))
	}
	if move && len(removals) != 0 {
		s.mailbox.messages = retained
		publishLocked(s.mailbox, advanceLocked(s.mailbox, origin, removals))
	}
	return &imap.CopyData{
		HasUIDs:         len(sources) != 0,
		UIDValidity:     destinationMailbox.uidValidity,
		SourceUIDs:      imap.UIDSetNum(sources...),
		DestinationUIDs: imap.UIDSetNum(destinations...),
	}, nil
}

func (s *selected) Expunge(ctx context.Context, writer *imapserver.ExpungeWriter, uids *imap.UIDSet, options *imapserver.ExpungeOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.readOnly {
		return noError(imap.CodeReadOnly, "mailbox is read-only")
	}
	s.session.account.mu.Lock()
	if s.closed {
		s.session.account.mu.Unlock()
		return noError(imap.CodeClosed, "mailbox is no longer selected")
	}
	var origin imapserver.ChangeToken
	if options != nil {
		origin = options.Origin
	}
	var removed []imap.UID
	var changes []imapserver.Update
	retained := s.mailbox.messages[:0]
	for _, msg := range s.mailbox.messages {
		selected := uids == nil || uids.Contains(msg.uid)
		if selected && imap.ContainsFlag(msg.flags, imap.FlagDeleted) {
			removed = append(removed, msg.uid)
			recordVanishedLocked(s.mailbox, msg.uid)
			changes = append(changes, &imapserver.UpdateExpunge{UID: msg.uid})
			continue
		}
		retained = append(retained, msg)
	}
	s.mailbox.messages = retained
	if len(changes) != 0 {
		publishLocked(s.mailbox, advanceLocked(s.mailbox, origin, changes))
	}
	s.session.account.mu.Unlock()
	for _, uid := range removed {
		if err := writer.WriteExpunge(ctx, uid); err != nil {
			return err
		}
	}
	return nil
}

func (s *selected) Unselect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.session.account.mu.Lock()
	defer s.session.account.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	delete(s.mailbox.watchers, s)
	delete(s.session.selections, s)
	return nil
}

func revision(value uint64) imapserver.MailboxRevision {
	return imapserver.MailboxRevision(strconv.FormatUint(value, 10))
}

func advanceLocked(m *mailbox, origin imapserver.ChangeToken, changes []imapserver.Update) *imapserver.UpdateBatch {
	before := revision(m.revision)
	m.revision++
	return &imapserver.UpdateBatch{Before: before, After: revision(m.revision), Origin: origin, Changes: changes}
}

func publishLocked(m *mailbox, batch *imapserver.UpdateBatch) {
	for _, updater := range m.watchers {
		_ = updater.Push(batch)
	}
}

func cloneFlags(flags []imap.Flag) []imap.Flag { return append([]imap.Flag(nil), flags...) }

func applyStoreFlags(current []imap.Flag, store *imapserver.StoreFlags) ([]imap.Flag, error) {
	switch store.Op {
	case imapserver.StoreFlagsSet:
		return uniqueFlags(store.Flags), nil
	case imapserver.StoreFlagsAdd:
		return uniqueFlags(append(cloneFlags(current), store.Flags...)), nil
	case imapserver.StoreFlagsRemove:
		out := cloneFlags(current)
		for _, remove := range store.Flags {
			out = slices.DeleteFunc(out, func(flag imap.Flag) bool { return flag.Equal(remove) })
		}
		return out, nil
	default:
		return nil, noError(imap.CodeClientBug, fmt.Sprintf("unknown STORE operation %q", store.Op))
	}
}

func uniqueFlags(flags []imap.Flag) []imap.Flag {
	var out []imap.Flag
	for _, flag := range flags {
		if !imap.ContainsFlag(out, flag) && !flag.Equal(imap.FlagRecent) {
			out = append(out, flag)
		}
	}
	return out
}

var (
	_ imapserver.SelectedMailbox = (*selected)(nil)
	_ imapserver.MoveMailbox     = (*selected)(nil)
)

// flagsFetchData is the untagged FETCH a STORE reports for one message.
//
// MODSEQ is always included: only this backend knows the value, and the
// framework removes it again for a session that has not enabled CONDSTORE.
// RFC 7162 section 3.1.4.2.
func flagsFetchData(seqNum imap.SeqNum, msg *message) *imap.FetchMessageData {
	return &imap.FetchMessageData{SeqNum: seqNum, Items: map[imap.FetchDataKey][]imap.FetchData{
		"FLAGS":  {imap.FetchDataFlags(cloneFlags(msg.flags))},
		"UID":    {imap.FetchDataUID(msg.uid)},
		"MODSEQ": {imap.FetchDataModSeq(msg.modSeq)},
	}}
}

func fetchMessageData(msg *message, seqNum imap.SeqNum, items []imap.FetchItem) (*imap.FetchMessageData, bool, error) {
	data := &imap.FetchMessageData{SeqNum: seqNum, Items: make(map[imap.FetchDataKey][]imap.FetchData)}
	marksSeen := false
	for _, item := range items {
		switch item := item.(type) {
		case imap.FetchItemKeyword:
			key := imap.FetchDataKey(strings.ToUpper(string(item)))
			switch item {
			case imap.FetchItemUID:
				data.Items[key] = append(data.Items[key], imap.FetchDataUID(msg.uid))
			case imap.FetchItemFlags:
				data.Items[key] = append(data.Items[key], imap.FetchDataFlags(cloneFlags(msg.flags)))
			case imap.FetchItemModSeq:
				data.Items[key] = append(data.Items[key], imap.FetchDataModSeq(msg.modSeq))
			case imap.FetchItemInternalDate:
				data.Items[key] = append(data.Items[key], &imap.FetchDataInternalDate{Time: msg.internalDate})
			case imap.FetchItemRFC822Size:
				data.Items[key] = append(data.Items[key], imap.FetchDataRFC822Size(len(msg.raw)))
			case imap.FetchItemEnvelope:
				data.Items[key] = append(data.Items[key], &imap.FetchDataEnvelope{Envelope: msg.analysis.Envelope})
			case imap.FetchItemRFC822:
				data.Items[key] = append(data.Items[key], &imap.FetchDataLiteral{Literal: bytes.NewReader(msg.raw)})
				marksSeen = true
			case imap.FetchItemRFC822Header, imap.FetchItemRFC822Text:
				specifier := imap.PartSpecifierHeader
				if item == imap.FetchItemRFC822Text {
					specifier = imap.PartSpecifierText
					marksSeen = true
				}
				reader, _, err := msg.analysis.OpenBodySection(&imap.FetchItemBodySection{Specifier: specifier, Peek: item == imap.FetchItemRFC822Header})
				if err != nil {
					return nil, false, err
				}
				data.Items[key] = append(data.Items[key], &imap.FetchDataLiteral{Literal: reader})
			default:
				return nil, false, noError(imap.CodeCannot, fmt.Sprintf("unsupported FETCH item %q", item))
			}
		case *imap.FetchItemBodyStructure:
			key := imap.FetchDataKey("BODY")
			if item.Extended {
				key = "BODYSTRUCTURE"
			}
			data.Items[key] = append(data.Items[key], &imap.FetchDataBodyStructure{BodyStructure: msg.analysis.BodyStructure})
		case *imap.FetchItemBodySection:
			reader, _, err := msg.analysis.OpenBodySection(item)
			if err != nil {
				return nil, false, err
			}
			key := imap.FetchDataKey(bodySectionKey(item))
			section := &imap.FetchDataBodySection{
				Part:            append([]int(nil), item.Part...),
				Specifier:       item.Specifier,
				HeaderFields:    append([]string(nil), item.HeaderFields...),
				HeaderFieldsNot: append([]string(nil), item.HeaderFieldsNot...),
				Literal:         reader,
			}
			if item.Partial != nil {
				section.Origin, section.HasOrigin = item.Partial.Offset, true
			}
			data.Items[key] = append(data.Items[key], section)
			marksSeen = marksSeen || !item.Peek
		default:
			return nil, false, noError(imap.CodeCannot, fmt.Sprintf("unsupported FETCH item %T", item))
		}
	}
	return data, marksSeen, nil
}

func bodySectionKey(item *imap.FetchItemBodySection) string {
	var b strings.Builder
	b.WriteString("BODY[")
	for i, number := range item.Part {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.Itoa(number))
	}
	specifier := string(item.Specifier)
	fields := item.HeaderFields
	if len(fields) != 0 {
		specifier = "HEADER.FIELDS"
	} else if len(item.HeaderFieldsNot) != 0 {
		specifier = "HEADER.FIELDS.NOT"
		fields = item.HeaderFieldsNot
	}
	if specifier != "" {
		if len(item.Part) > 0 {
			b.WriteByte('.')
		}
		b.WriteString(specifier)
		if len(fields) != 0 {
			b.WriteString(" (")
			for i, field := range fields {
				if i > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(field)
			}
			b.WriteByte(')')
		}
	}
	b.WriteByte(']')
	if item.Partial != nil {
		fmt.Fprintf(&b, "<%d>", item.Partial.Offset)
	}
	return b.String()
}
