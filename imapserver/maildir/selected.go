package maildir

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/message"
)

func (s *selected) Store(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, flags *imapserver.StoreFlags, options *imapserver.StoreOptions) error {
	_, err := s.StoreCondStore(ctx, writer, uids, flags, options)
	return err
}
func (s *selected) StoreCondStore(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, store *imapserver.StoreFlags, options *imapserver.StoreOptions) (*imapserver.CondStoreResult, error) {
	name, _, err := s.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer s.session.account.leave()
	if s.readOnly {
		return nil, noError(imap.CodeReadOnly, "mailbox is read-only")
	}
	if store == nil {
		return nil, noError(imap.CodeClientBug, "nil STORE flags")
	}
	if _, err := applyFlags(nil, store); err != nil {
		return nil, err
	}
	a := s.session.account
	next, err := cloneState(a.state)
	if err != nil {
		return nil, err
	}
	m := next.Mailboxes[name]
	var changes []imapserver.Update
	var actions []fileAction
	var results []*imap.FetchMessageData
	var modified []imap.UID
	var origin imapserver.ChangeToken
	silent, conditional := false, false
	var bound uint64
	if options != nil {
		origin = options.Origin
		silent = options.Silent
		conditional = options.HasUnchangedSince
		bound = options.UnchangedSince
	}
	for i, msg := range m.Messages {
		if !uids.Contains(msg.UID) {
			continue
		}
		if conditional && msg.ModSeq > bound {
			modified = append(modified, msg.UID)
			continue
		}
		flags, err := applyFlags(msg.Flags, store)
		if err != nil {
			return nil, err
		}
		if !sameFlags(msg.Flags, flags) {
			old := messagePath(name, msg)
			msg.Flags = flags
			modseq, err := bump(m)
			if err != nil {
				return nil, err
			}
			msg.ModSeq = modseq
			base, _, _ := strings.Cut(msg.File, ":2,")
			msg.File = base + flagSuffix(flags)
			if old != messagePath(name, msg) {
				actions = append(actions, fileAction{Kind: "rename", From: old, To: messagePath(name, msg)})
			}
			if ensureFlags(m, flags) {
				changes = withMailboxFlags(changes, m.Flags)
			}
			changes = append(changes, &imapserver.UpdateFlags{UID: msg.UID, Flags: slices.Clone(flags), ModSeq: msg.ModSeq})
		}
		if !silent || conditional {
			results = append(results, flagsData(imap.SeqNum(i+1), msg))
		}
	}
	if len(changes) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := a.publishCommit(next, actions, map[uint32][]imapserver.Update{s.id: changes}, origin); err != nil {
			return nil, err
		}
	}
	for _, data := range results {
		if err := writer.WriteMessage(ctx, data); err != nil {
			return nil, err
		}
	}
	return &imapserver.CondStoreResult{Modified: imap.UIDSetNum(modified...), HighestModSeq: m.HighestModSeq}, nil
}
func applyFlags(current []imap.Flag, store *imapserver.StoreFlags) ([]imap.Flag, error) {
	switch store.Op {
	case imapserver.StoreFlagsSet:
		return uniqueFlags(store.Flags), nil
	case imapserver.StoreFlagsAdd:
		return uniqueFlags(append(slices.Clone(current), store.Flags...)), nil
	case imapserver.StoreFlagsRemove:
		out := slices.Clone(current)
		for _, remove := range store.Flags {
			out = slices.DeleteFunc(out, func(f imap.Flag) bool { return f.Equal(remove) })
		}
		return out, nil
	}
	return nil, noError(imap.CodeClientBug, "unknown STORE operation")
}
func sameFlags(a, b []imap.Flag) bool {
	if len(a) != len(b) {
		return false
	}
	for _, f := range a {
		if !imap.ContainsFlag(b, f) {
			return false
		}
	}
	return true
}
func flagsData(seq imap.SeqNum, msg *messageRecord) *imap.FetchMessageData {
	return &imap.FetchMessageData{SeqNum: seq, Items: map[imap.FetchDataKey][]imap.FetchData{"UID": {imap.FetchDataUID(msg.UID)}, "FLAGS": {imap.FetchDataFlags(slices.Clone(msg.Flags))}, "MODSEQ": {imap.FetchDataModSeq(msg.ModSeq)}}}
}

func (s *selected) Copy(ctx context.Context, uids imap.UIDSet, destination string, options *imapserver.CopyOptions) (*imap.CopyData, error) {
	var origin imapserver.ChangeToken
	if options != nil {
		origin = options.Origin
	}
	return s.copyMove(ctx, uids, destination, origin, false)
}
func (s *selected) Move(ctx context.Context, uids imap.UIDSet, destination string, options *imapserver.MoveOptions) (*imap.CopyData, error) {
	var origin imapserver.ChangeToken
	if options != nil {
		origin = options.Origin
	}
	return s.copyMove(ctx, uids, destination, origin, true)
}
func (s *selected) copyMove(ctx context.Context, uids imap.UIDSet, destination string, origin imapserver.ChangeToken, move bool) (*imap.CopyData, error) {
	name, source, err := s.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer s.session.account.leave()
	if move && s.readOnly {
		return nil, noError(imap.CodeReadOnly, "mailbox is read-only")
	}
	destination = mailboxKey(destination)
	a := s.session.account
	if a.state.Mailboxes[destination] == nil {
		return nil, noError(imap.CodeTryCreate, "destination mailbox does not exist")
	}
	if move && name == destination {
		return nil, noError(imap.CodeCannot, "cannot MOVE to the selected mailbox")
	}
	next, err := cloneState(a.state)
	if err != nil {
		return nil, err
	}
	from, to := next.Mailboxes[name], next.Mailboxes[destination]
	var actions []fileAction
	changes := make(map[uint32][]imapserver.Update)
	var sourceUIDs, destinationUIDs []imap.UID
	var retained []*messageRecord
	for _, original := range source.Messages {
		if !uids.Contains(original.UID) {
			if move {
				for _, msg := range from.Messages {
					if msg.UID == original.UID {
						retained = append(retained, msg)
						break
					}
				}
			}
			continue
		}
		uid, err := allocateUID(to)
		if err != nil {
			return nil, err
		}
		modseq, err := bump(to)
		if err != nil {
			return nil, err
		}
		f, err := openRegular(a.root, messagePath(name, original))
		if err != nil {
			return nil, err
		}
		stage, size, date, err := a.stage(ctx, f, original.Date, false)
		f.Close()
		if err != nil {
			return nil, err
		}
		defer a.discardStage(stage)
		msg := *original
		msg.UID = uid
		msg.File = filepath.Base(stage) + flagSuffix(original.Flags)
		msg.Size = size
		msg.Date = date
		msg.ModSeq = modseq
		msg.Flags = slices.Clone(original.Flags)
		to.Messages = append(to.Messages, &msg)
		if ensureFlags(to, msg.Flags) {
			changes[to.UIDValidity] = withMailboxFlags(changes[to.UIDValidity], to.Flags)
		}
		changes[to.UIDValidity] = append(changes[to.UIDValidity], &imapserver.UpdateAdd{UIDs: []imap.UID{uid}})
		actions = append(actions, fileAction{Kind: "rename", From: stage, To: messagePath(destination, &msg)})
		if move {
			modseq, err := bump(from)
			if err != nil {
				return nil, err
			}
			from.Tombstones = append(from.Tombstones, tombstone{UID: original.UID, ModSeq: modseq})
			changes[from.UIDValidity] = append(changes[from.UIDValidity], &imapserver.UpdateExpunge{UID: original.UID})
			actions = append(actions, fileAction{Kind: "remove", From: messagePath(name, original)})
		}
		sourceUIDs = append(sourceUIDs, original.UID)
		destinationUIDs = append(destinationUIDs, uid)
	}
	if move {
		from.Messages = retained
	}
	if len(sourceUIDs) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := a.publishCommit(next, actions, changes, origin); err != nil {
			return nil, err
		}
	}
	return &imap.CopyData{HasUIDs: len(sourceUIDs) > 0, UIDValidity: to.UIDValidity, SourceUIDs: imap.UIDSetNum(sourceUIDs...), DestinationUIDs: imap.UIDSetNum(destinationUIDs...)}, nil
}

func (s *selected) Expunge(ctx context.Context, writer *imapserver.ExpungeWriter, uids *imap.UIDSet, options *imapserver.ExpungeOptions) error {
	name, _, err := s.enter(ctx)
	if err != nil {
		return err
	}
	defer s.session.account.leave()
	if s.readOnly {
		return noError(imap.CodeReadOnly, "mailbox is read-only")
	}
	a := s.session.account
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	m := next.Mailboxes[name]
	var retained []*messageRecord
	var removed []imap.UID
	var changes []imapserver.Update
	var actions []fileAction
	var origin imapserver.ChangeToken
	if options != nil {
		origin = options.Origin
	}
	for _, msg := range m.Messages {
		if (uids == nil || uids.Contains(msg.UID)) && imap.ContainsFlag(msg.Flags, imap.FlagDeleted) {
			modseq, err := bump(m)
			if err != nil {
				return err
			}
			m.Tombstones = append(m.Tombstones, tombstone{UID: msg.UID, ModSeq: modseq})
			removed = append(removed, msg.UID)
			changes = append(changes, &imapserver.UpdateExpunge{UID: msg.UID})
			actions = append(actions, fileAction{Kind: "remove", From: messagePath(name, msg)})
		} else {
			retained = append(retained, msg)
		}
	}
	m.Messages = retained
	if len(removed) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.publishCommit(next, actions, map[uint32][]imapserver.Update{s.id: changes}, origin); err != nil {
			return err
		}
	}
	for _, uid := range removed {
		if err := writer.WriteExpunge(ctx, uid, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *selected) Resync(ctx context.Context, params *imapserver.QResyncSelect, _ *imapserver.QResyncOptions) (*imapserver.QResyncResult, error) {
	_, m, err := s.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer s.session.account.leave()
	result := &imapserver.QResyncResult{}
	if params == nil || params.UIDValidity != m.UIDValidity {
		return result, nil
	}
	for _, dead := range m.Tombstones {
		if dead.ModSeq > params.ModSeq && (params.KnownUIDs.IsEmpty() || params.KnownUIDs.Contains(dead.UID)) {
			result.Vanished.AddNum(dead.UID)
		}
	}
	for _, msg := range m.Messages {
		if msg.ModSeq > params.ModSeq && (params.KnownUIDs.IsEmpty() || params.KnownUIDs.Contains(msg.UID)) {
			result.Changed.AddNum(msg.UID)
		}
	}
	return result, nil
}

func (s *selected) Search(ctx context.Context, query *imapserver.SearchQuery, options *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	name, m, err := s.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer s.session.account.leave()
	criteria := imap.SearchCriteria(imap.SearchAll)
	if query != nil && query.Criteria() != nil {
		criteria = query.Criteria()
	}
	charset := ""
	if options != nil {
		charset = options.Charset
	}
	opts := &message.MatchOptions{Charset: charset}
	// Validate unsupported keys/charsets even when the mailbox has no messages.
	empty, err := message.Analyze(ctx, bytes.NewReader(nil), 0, nil)
	if err != nil {
		return nil, err
	}
	if _, err := message.Match(ctx, empty, message.Metadata{}, criteria, opts); err != nil {
		return nil, err
	}
	result := &imapserver.SearchResult{}
	for i, msg := range m.Messages {
		f, err := openRegular(s.session.account.root, messagePath(name, msg))
		if err != nil {
			return nil, err
		}
		analysis, err := message.Analyze(ctx, f, msg.Size, nil)
		if err != nil {
			f.Close()
			return nil, err
		}
		matched, err := message.Match(ctx, analysis, message.Metadata{SeqNum: imap.SeqNum(i + 1), UID: msg.UID, Flags: slices.Clone(msg.Flags), InternalDate: msg.Date, RFC822Size: msg.Size, ModSeq: msg.ModSeq}, criteria, opts)
		f.Close()
		if err != nil {
			return nil, err
		}
		if matched {
			result.UIDs = append(result.UIDs, msg.UID)
			result.ModSeq = max(result.ModSeq, msg.ModSeq)
		}
	}
	return result, nil
}

func (s *selected) Fetch(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, options *imapserver.FetchOptions) error {
	name, m, err := s.enter(ctx)
	if err != nil {
		return err
	}
	defer s.session.account.leave()
	items := []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags}
	var changed uint64
	if options != nil {
		if len(options.Items) > 0 {
			items = options.Items
		}
		changed = options.ChangedSince
	}
	// Keep only the immutable UID list; committing a Seen flag replaces state.
	var selectedUIDs []imap.UID
	for _, msg := range m.Messages {
		if uids.Contains(msg.UID) && msg.ModSeq > changed {
			selectedUIDs = append(selectedUIDs, msg.UID)
		}
	}
	marksSeen, err := fetchMarksSeen(items)
	if err != nil {
		return err
	}
	// The implicit \Seen is one commit and one update batch for the whole
	// FETCH, before any response is written. One per message pushed a batch
	// per message into every selected session's queue, which the framework
	// drains only after the FETCH: past MaxQueuedUpdates the connection was
	// closed. It also rewrote the whole account state once per message.
	if marksSeen && !s.readOnly {
		var unseen []imap.UID
		fetched := make(map[imap.UID]bool, len(selectedUIDs))
		for _, uid := range selectedUIDs {
			fetched[uid] = true
		}
		for _, msg := range m.Messages {
			if fetched[msg.UID] && !imap.ContainsFlag(msg.Flags, imap.FlagSeen) {
				unseen = append(unseen, msg.UID)
			}
		}
		if len(unseen) > 0 {
			// A supported item type can still name an unsupported section or a
			// missing part. Use the response builder itself to validate every
			// targeted message before changing any flags. Open one file at a
			// time and discard its streaming readers without reading the body.
			for i, msg := range m.Messages {
				if fetched[msg.UID] {
					if err := s.fetchMessage(ctx, nil, name, msg, imap.SeqNum(i+1), items); err != nil {
						return err
					}
				}
			}
		}
		if err := s.markSeen(ctx, name, unseen); err != nil {
			return err
		}
	}
	m = s.session.account.state.Mailboxes[name]
	for _, uid := range selectedUIDs {
		at := slices.IndexFunc(m.Messages, func(msg *messageRecord) bool { return msg.UID == uid })
		msg := m.Messages[at]
		if err := s.fetchMessage(ctx, writer, name, msg, imap.SeqNum(at+1), items); err != nil {
			return err
		}
	}
	return nil
}

// fetchMessage builds one response, keeping its file open until the writer
// returns. A nil writer validates the request without emitting a response.
func (s *selected) fetchMessage(ctx context.Context, writer *imapserver.FetchWriter, name string, msg *messageRecord, seq imap.SeqNum, items []imap.FetchItem) error {
	f, err := openRegular(s.session.account.root, messagePath(name, msg))
	if err != nil {
		return err
	}
	defer f.Close()
	analysis, err := message.Analyze(ctx, f, msg.Size, nil)
	if err != nil {
		return err
	}
	data, _, err := fetchData(ctx, msg, analysis, seq, items)
	if err != nil {
		return err
	}
	if writer != nil {
		return writer.WriteMessage(ctx, data)
	}
	return nil
}

// fetchMarksSeen checks supported item types and reports whether they set
// \Seen. Section arguments also need per-message validation via fetchData
// before any side effect.
func fetchMarksSeen(items []imap.FetchItem) (bool, error) {
	seen := false
	for _, item := range items {
		switch item := item.(type) {
		case imap.FetchItemKeyword:
			switch item {
			case imap.FetchItemUID, imap.FetchItemFlags, imap.FetchItemModSeq, imap.FetchItemInternalDate,
				imap.FetchItemRFC822Size, imap.FetchItemEnvelope, imap.FetchItemRFC822Header:
			case imap.FetchItemRFC822, imap.FetchItemRFC822Text:
				seen = true
			default:
				return false, noError(imap.CodeCannot, fmt.Sprintf("unsupported FETCH item %q", item))
			}
		case *imap.FetchItemBodyStructure:
		case *imap.FetchItemBodySection:
			seen = seen || !item.Peek
		default:
			return false, noError(imap.CodeCannot, fmt.Sprintf("unsupported FETCH item %T", item))
		}
	}
	return seen, nil
}

func (s *selected) markSeen(ctx context.Context, name string, uids []imap.UID) error {
	if len(uids) == 0 {
		return nil
	}
	a := s.session.account
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	m := next.Mailboxes[name]
	mark := make(map[imap.UID]bool, len(uids))
	for _, uid := range uids {
		mark[uid] = true
	}
	var actions []fileAction
	var changes []imapserver.Update
	for _, msg := range m.Messages {
		if !mark[msg.UID] {
			continue
		}
		old := messagePath(name, msg)
		msg.Flags = append(msg.Flags, imap.FlagSeen)
		modseq, err := bump(m)
		if err != nil {
			return err
		}
		msg.ModSeq = modseq
		base, _, _ := strings.Cut(msg.File, ":2,")
		msg.File = base + flagSuffix(msg.Flags)
		actions = append(actions, fileAction{Kind: "rename", From: old, To: messagePath(name, msg)})
		changes = append(changes, &imapserver.UpdateFlags{UID: msg.UID, Flags: slices.Clone(msg.Flags), ModSeq: modseq})
	}
	if len(changes) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.publishCommit(next, actions, map[uint32][]imapserver.Update{s.id: changes}, 0)
}

type sizedReader struct {
	io.Reader
	remaining int
}

func (r *sizedReader) Len() int { return r.remaining }
func (r *sizedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.remaining -= n
	return n, err
}
func sized(reader io.Reader, n int64) (io.Reader, error) {
	if n < 0 || int64(int(n)) != n {
		return nil, noError(imap.CodeLimit, "literal exceeds platform size limit")
	}
	return &sizedReader{Reader: reader, remaining: int(n)}, nil
}

func fetchData(ctx context.Context, msg *messageRecord, analysis *message.Message, seq imap.SeqNum, items []imap.FetchItem) (*imap.FetchMessageData, bool, error) {
	data := &imap.FetchMessageData{SeqNum: seq, Items: make(map[imap.FetchDataKey][]imap.FetchData)}
	seen := false
	for _, item := range items {
		switch item := item.(type) {
		case imap.FetchItemKeyword:
			key := imap.FetchDataKey(strings.ToUpper(string(item)))
			var value imap.FetchData
			switch item {
			case imap.FetchItemUID:
				value = imap.FetchDataUID(msg.UID)
			case imap.FetchItemFlags:
				value = imap.FetchDataFlags(slices.Clone(msg.Flags))
			case imap.FetchItemModSeq:
				value = imap.FetchDataModSeq(msg.ModSeq)
			case imap.FetchItemInternalDate:
				value = &imap.FetchDataInternalDate{Time: msg.Date}
			case imap.FetchItemRFC822Size:
				value = imap.FetchDataRFC822Size(msg.Size)
			case imap.FetchItemEnvelope:
				value = &imap.FetchDataEnvelope{Envelope: analysis.Envelope()}
			case imap.FetchItemRFC822, imap.FetchItemRFC822Header, imap.FetchItemRFC822Text:
				section := &imap.FetchItemBodySection{Peek: item == imap.FetchItemRFC822Header}
				if item == imap.FetchItemRFC822Header {
					section.Specifier = imap.PartSpecifierHeader
				}
				if item == imap.FetchItemRFC822Text {
					section.Specifier = imap.PartSpecifierText
				}
				reader, n, err := analysis.OpenBodySection(ctx, section)
				if err != nil {
					return nil, false, err
				}
				reader, err = sized(reader, n)
				if err != nil {
					return nil, false, err
				}
				value = &imap.FetchDataLiteral{Literal: reader}
				seen = seen || !section.Peek
			default:
				return nil, false, noError(imap.CodeCannot, fmt.Sprintf("unsupported FETCH item %q", item))
			}
			data.Items[key] = append(data.Items[key], value)
		case *imap.FetchItemBodyStructure:
			key := imap.FetchDataKey("BODY")
			if item.Extended {
				key = "BODYSTRUCTURE"
			}
			data.Items[key] = append(data.Items[key], &imap.FetchDataBodyStructure{BodyStructure: analysis.BodyStructure()})
		case *imap.FetchItemBodySection:
			reader, n, err := analysis.OpenBodySection(ctx, item)
			if err != nil {
				return nil, false, err
			}
			reader, err = sized(reader, n)
			if err != nil {
				return nil, false, err
			}
			value := &imap.FetchDataBodySection{Part: slices.Clone(item.Part), Specifier: item.Specifier, HeaderFields: slices.Clone(item.HeaderFields), HeaderFieldsNot: slices.Clone(item.HeaderFieldsNot), Literal: reader}
			if item.Partial != nil {
				value.HasOrigin = true
				value.Origin = item.Partial.Offset
			}
			key := imap.FetchDataKey(bodySectionKey(item))
			data.Items[key] = append(data.Items[key], value)
			seen = seen || !item.Peek
		default:
			return nil, false, noError(imap.CodeCannot, fmt.Sprintf("unsupported FETCH item %T", item))
		}
	}
	return data, seen, nil
}
func bodySectionKey(item *imap.FetchItemBodySection) string {
	var b strings.Builder
	b.WriteString("BODY[")
	for i, n := range item.Part {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.Itoa(n))
	}
	specifier := string(item.Specifier)
	fields := item.HeaderFields
	if len(fields) > 0 {
		specifier = "HEADER.FIELDS"
	} else if len(item.HeaderFieldsNot) > 0 {
		specifier = "HEADER.FIELDS.NOT"
		fields = item.HeaderFieldsNot
	}
	if specifier != "" {
		if len(item.Part) > 0 {
			b.WriteByte('.')
		}
		b.WriteString(specifier)
		if len(fields) > 0 {
			b.WriteString(" (")
			b.WriteString(strings.Join(fields, " "))
			b.WriteByte(')')
		}
	}
	b.WriteByte(']')
	if item.Partial != nil {
		fmt.Fprintf(&b, "<%d>", item.Partial.Offset)
	}
	return b.String()
}

var _ imapserver.SelectedMailbox = (*selected)(nil)
var _ imapserver.MoveMailbox = (*selected)(nil)
var _ imapserver.CondStoreMailbox = (*selected)(nil)
var _ imapserver.QResyncMailbox = (*selected)(nil)
