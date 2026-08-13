package imapserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

const (
	maxCommandListResults   = 100_000
	maxCommandSearchResults = 1_000_000
	maxCommandFetchBytes    = 256 << 20
)

var commandOrigin atomic.Uint64

type uidCommandArgs struct {
	descriptor *commandDescriptor
	args       any
}

var uidCommandDescriptors = map[string]*commandDescriptor{
	"FETCH":  {name: "FETCH", states: stateMaskSelected, parse: parseFetch, handle: handleFetch},
	"STORE":  {name: "STORE", states: stateMaskSelected, parse: parseStore, handle: handleStore},
	"SEARCH": {name: "SEARCH", states: stateMaskSelected, parse: parseSearch, handle: handleSearch},
	"COPY":   {name: "COPY", states: stateMaskSelected, parse: parseCopy, handle: handleCopy},
	"MOVE":   {name: "MOVE", states: stateMaskSelected, parse: parseCopy, handle: handleMove},
}

func init() {
	registerCommand("LOGIN", stateMaskNotAuthenticated, true, parseLogin, handleLogin)
	registerCommand("AUTHENTICATE", stateMaskNotAuthenticated, true, parseAuthenticate, handleAuthenticate)
	registerCommand("LIST", stateMaskAuthenticated|stateMaskSelected, false, parseList, handleList)
	registerCommand("LSUB", stateMaskAuthenticated|stateMaskSelected, false, parseLsub, handleList)
	registerCommand("STATUS", stateMaskAuthenticated|stateMaskSelected, false, parseStatus, handleStatus)
	registerCommand("CREATE", stateMaskAuthenticated|stateMaskSelected, false, parseMailbox, handleCreate)
	registerCommand("DELETE", stateMaskAuthenticated|stateMaskSelected, false, parseMailbox, handleDelete)
	registerCommand("RENAME", stateMaskAuthenticated|stateMaskSelected, false, parseRename, handleRename)
	registerCommand("SUBSCRIBE", stateMaskAuthenticated|stateMaskSelected, false, parseMailbox, handleSubscribe)
	registerCommand("UNSUBSCRIBE", stateMaskAuthenticated|stateMaskSelected, false, parseMailbox, handleUnsubscribe)
	registerCommand("SELECT", stateMaskAuthenticated|stateMaskSelected, true, parseMailbox, handleSelect)
	registerCommand("EXAMINE", stateMaskAuthenticated|stateMaskSelected, true, parseMailbox, handleSelect)
	registerCommand("APPEND", stateMaskAuthenticated|stateMaskSelected, true, parseAppend, handleAppend)
	registerCommand("FETCH", stateMaskSelected, false, parseFetch, handleFetch)
	registerCommand("STORE", stateMaskSelected, false, parseStore, handleStore)
	registerCommand("SEARCH", stateMaskSelected, false, parseSearch, handleSearch)
	registerCommand("COPY", stateMaskSelected, false, parseCopy, handleCopy)
	registerCommand("MOVE", stateMaskSelected, false, parseCopy, handleMove)
	registerCommand("EXPUNGE", stateMaskSelected, false, parseNoArgs, handleExpunge)
	registerCommand("CLOSE", stateMaskSelected, true, parseNoArgs, handleClose)
	registerCommand("IDLE", stateMaskAuthenticated|stateMaskSelected, true, parseNoArgs, handleIdle)
	registerCommand("UID", stateMaskSelected, false, parseUIDCommand, handleUIDCommand)
}

func registerCommand(name string, states stateMask, barrier bool, parse func(*imapwire.Decoder) (any, int64, error), handle func(context.Context, *conn, *queuedCommand) error) {
	if _, exists := commandDescriptors[name]; exists {
		panic("imapserver: duplicate command descriptor for " + name)
	}
	commandDescriptors[name] = &commandDescriptor{name: name, states: states, barrier: barrier, parse: parse, handle: handle}
}

func parseUIDCommand(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	var name string
	if !decoder.ExpectAtom(&name) {
		return nil, 0, decoder.Err()
	}
	name = strings.ToUpper(name)
	descriptor := uidCommandDescriptors[name]
	if descriptor == nil {
		return nil, 0, fmt.Errorf("unsupported UID subcommand %q", name)
	}
	args, size, err := descriptor.parse(decoder)
	return &uidCommandArgs{descriptor: descriptor, args: args}, size + int64(len(name)+1), err
}

func handleUIDCommand(ctx context.Context, c *conn, command *queuedCommand) error {
	args, ok := command.args.(*uidCommandArgs)
	if !ok || args == nil || args.descriptor == nil {
		return c.writeBad(command.tag, "invalid UID command")
	}
	nested := *command
	nested.name = "UID " + args.descriptor.name
	nested.descriptor = args.descriptor
	nested.args = args.args
	return args.descriptor.handle(ctx, c, &nested)
}

func nextCommandOrigin() ChangeToken {
	for {
		origin := ChangeToken(commandOrigin.Add(1))
		if origin != 0 {
			return origin
		}
	}
}

func writeTaggedCondition(c *conn, tag, status string, code imap.ResponseCode, args, textValue string) error {
	if textValue == "" {
		textValue = "command failed"
	}
	if strings.ContainsAny(textValue, "\x00\r\n") {
		textValue = "command failed"
	}
	codeText := strings.ToUpper(string(code))
	if strings.ContainsAny(codeText, "[] \t\r\n") || strings.ContainsAny(args, "]\x00\r\n") {
		codeText, args = string(imap.CodeServerBug), ""
	}
	c.encoder.BeginResponse(imapwire.ResponseTagged, tag).RespCond(imapwire.RespCond{
		Status: status,
		Text:   imapwire.RespText{Code: codeText, Args: args, Text: textValue},
	}).CRLF()
	return c.encoder.Flush()
}

func writeBackendError(c *conn, tag, operation string, err error) error {
	var protocolErr *imap.Error
	if errors.As(err, &protocolErr) {
		status := strings.ToUpper(string(protocolErr.Type))
		if status != "NO" && status != "BAD" {
			status = "NO"
		}
		textValue := protocolErr.Text
		if textValue == "" {
			textValue = operation + " failed"
		}
		return writeTaggedCondition(c, tag, status, protocolErr.Code, protocolErr.CodeArgs, textValue)
	}
	code := imap.CodeServerBug
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		code = imap.CodeUnavailable
	}
	return writeTaggedCondition(c, tag, "NO", code, "", operation+" failed")
}

func commandLimitError(textValue string) error {
	return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeLimit, Text: textValue}
}

func parseMailbox(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	var mailbox string
	if !decoder.ExpectMailbox(&mailbox) || !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return mailbox, int64(len(mailbox)), nil
}

func commandUsesUIDs(command *queuedCommand) bool {
	return command != nil && strings.HasPrefix(command.name, "UID ")
}

func resolveMessageSet(selected *selectedState, raw string, uidMode bool) (imap.UIDSet, []imap.UID, error) {
	if selected == nil || raw == "" {
		return nil, nil, fmt.Errorf("message set requires a selected mailbox")
	}
	// SEARCHRES lets "$" stand in for the last saved result anywhere a message
	// set is accepted, in both the sequence-number and UID number spaces.
	// See ext_a_esearch.go.
	if raw == searchResultMarker {
		set, ordered := resolveSavedSearch(selected)
		return set, ordered, nil
	}
	var ordered []imap.UID
	if uidMode {
		set, err := imap.ParseUIDSet(raw)
		if err != nil {
			return nil, nil, err
		}
		maximum := imap.UID(0)
		if len(selected.uids) != 0 {
			maximum = selected.uids[len(selected.uids)-1]
		}
		for _, uid := range selected.uids {
			if uidSetContains(set, uid, maximum) {
				ordered = append(ordered, uid)
			}
		}
	} else {
		set, err := imap.ParseSeqSet(raw)
		if err != nil {
			return nil, nil, err
		}
		maximum := imap.SeqNum(len(selected.uids))
		for i, uid := range selected.uids {
			if seqSetContains(set, imap.SeqNum(i+1), maximum) {
				ordered = append(ordered, uid)
			}
		}
	}
	return imap.UIDSetNum(ordered...), ordered, nil
}

func uidSetContains(set imap.UIDSet, uid, maximum imap.UID) bool {
	for _, r := range set {
		start, stop := r.Start, r.Stop
		if start == 0 {
			start = maximum
		}
		if stop == 0 {
			stop = maximum
		}
		if start > stop {
			start, stop = stop, start
		}
		if start != 0 && start <= uid && uid <= stop {
			return true
		}
	}
	return false
}

func seqSetContains(set imap.SeqSet, seq, maximum imap.SeqNum) bool {
	for _, r := range set {
		start, stop := r.Start, r.Stop
		if start == 0 {
			start = maximum
		}
		if stop == 0 {
			stop = maximum
		}
		if start > stop {
			start, stop = stop, start
		}
		if start != 0 && start <= seq && seq <= stop {
			return true
		}
	}
	return false
}

func extractFetchUID(data *imap.FetchMessageData) (imap.UID, bool) {
	if data == nil {
		return 0, false
	}
	for key, values := range data.Items {
		if !strings.EqualFold(string(key), "UID") {
			continue
		}
		for _, value := range values {
			if uid, ok := value.(imap.FetchDataUID); ok && uid != 0 {
				return imap.UID(uid), true
			}
		}
	}
	return 0, false
}

func mapFetchResponse(selected *selectedState, data *imap.FetchMessageData, includeUID bool) (*imap.FetchMessageData, error) {
	uid, ok := extractFetchUID(data)
	if !ok {
		return nil, fmt.Errorf("imapserver: backend FETCH result omitted the framework-requested UID")
	}
	seqNum, ok := selected.sequence(uid)
	if !ok {
		return nil, fmt.Errorf("imapserver: backend FETCH result used unknown UID %d", uid)
	}
	copyData := *data
	copyData.SeqNum = seqNum
	copyData.Items = make(map[imap.FetchDataKey][]imap.FetchData, len(data.Items))
	for key, values := range data.Items {
		if !includeUID && strings.EqualFold(string(key), "UID") {
			continue
		}
		copyData.Items[key] = slices.Clone(values)
	}
	return &copyData, nil
}

func withFetchUID(items []imap.FetchItem) ([]imap.FetchItem, bool) {
	for _, item := range items {
		if keyword, ok := item.(imap.FetchItemKeyword); ok && strings.EqualFold(string(keyword), string(imap.FetchItemUID)) {
			return slices.Clone(items), true
		}
	}
	return append(slices.Clone(items), imap.FetchItemUID), false
}

func fetchLiteralReader(value imap.FetchData) io.Reader {
	switch value := value.(type) {
	case *imap.FetchDataLiteral:
		if value != nil {
			return value.Literal
		}
	case *imap.FetchDataBodySection:
		if value != nil {
			return value.Literal
		}
	case *imap.FetchDataBinarySection:
		if value != nil {
			return value.Literal
		}
	}
	return nil
}

func fetchLiteralSize(_ imap.FetchDataKey, value imap.FetchData) (int64, error) {
	reader := fetchLiteralReader(value)
	if reader == nil {
		return -1, fmt.Errorf("FETCH value is not a literal")
	}
	if sized, ok := reader.(interface{ Len() int }); ok {
		return int64(sized.Len()), nil
	}
	if seeker, ok := reader.(io.Seeker); ok {
		at, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return -1, err
		}
		end, err := seeker.Seek(0, io.SeekEnd)
		if err != nil {
			return -1, err
		}
		if _, err := seeker.Seek(at, io.SeekStart); err != nil {
			return -1, err
		}
		return end - at, nil
	}
	return -1, fmt.Errorf("FETCH literal reader %T does not expose its remaining size", reader)
}

// prepareFetchResponseLiterals makes every response literal measurable before
// the encoder announces it on the wire. Readers which expose a remaining size
// stay streaming; an opaque reader is copied to a bounded temporary file.
func prepareFetchResponseLiterals(data *imap.FetchMessageData, limit int64) (int64, func(), error) {
	var total int64
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	for key, values := range data.Items {
		for i, value := range values {
			reader := fetchLiteralReader(value)
			if reader == nil {
				if isFetchLiteralValue(value) {
					cleanup()
					return 0, func() {}, fmt.Errorf("FETCH %s has a nil literal reader", key)
				}
				continue
			}
			size, err := fetchLiteralSize(key, value)
			if err != nil {
				remaining := limit - total
				if remaining < 0 {
					cleanup()
					return 0, func() {}, commandLimitError("FETCH response byte limit exceeded")
				}
				staged, stagedSize, stagedCleanup, stageErr := stageFetchLiteral(reader, remaining)
				if stageErr != nil {
					cleanup()
					return 0, func() {}, stageErr
				}
				cleanups = append(cleanups, stagedCleanup)
				values[i], err = replaceFetchLiteralReader(value, staged)
				if err != nil {
					cleanup()
					return 0, func() {}, err
				}
				size = stagedSize
			} else if closer, ok := reader.(io.Closer); ok {
				cleanups = append(cleanups, func() { _ = closer.Close() })
			}
			if size < 0 || size > limit-total {
				cleanup()
				return 0, func() {}, commandLimitError("FETCH response byte limit exceeded")
			}
			total += size
		}
		data.Items[key] = values
	}
	return total, cleanup, nil
}

func fetchResponseWireSize(data *imap.FetchMessageData) (int64, error) {
	if data == nil {
		return 0, fmt.Errorf("imapserver: nil FETCH response")
	}
	copyData := *data
	copyData.Items = make(map[imap.FetchDataKey][]imap.FetchData, len(data.Items))
	for key, values := range data.Items {
		copyValues := slices.Clone(values)
		for i, value := range copyValues {
			if fetchLiteralReader(value) == nil {
				if _, raw := value.(*imap.FetchDataRaw); raw {
					return 0, fmt.Errorf("imapserver: raw FETCH values cannot be safely bounded")
				}
				continue
			}
			size, err := fetchLiteralSize(key, value)
			if err != nil {
				return 0, err
			}
			copyValues[i], err = replaceFetchLiteralReader(value, &sizedZeroReader{remaining: size})
			if err != nil {
				return 0, err
			}
		}
		copyData.Items[key] = copyValues
	}
	var counter countingWriter
	encoder := imapwire.NewEncoder(&counter, &imapwire.EncoderOptions{ServerResponse: true})
	if err := imapcodec.WriteFetchResponse(encoder, &copyData, fetchLiteralSize); err != nil {
		return 0, err
	}
	if err := encoder.Flush(); err != nil {
		return 0, err
	}
	return counter.bytes, nil
}

type countingWriter struct{ bytes int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	return len(p), nil
}

type sizedZeroReader struct{ remaining int64 }

func (r *sizedZeroReader) Len() int { return int(r.remaining) }

func (r *sizedZeroReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.remaining -= int64(len(p))
	return len(p), nil
}

func isFetchLiteralValue(value imap.FetchData) bool {
	switch value.(type) {
	case *imap.FetchDataLiteral, *imap.FetchDataBodySection, *imap.FetchDataBinarySection:
		return true
	default:
		return false
	}
}

func replaceFetchLiteralReader(value imap.FetchData, reader io.Reader) (imap.FetchData, error) {
	switch value := value.(type) {
	case *imap.FetchDataLiteral:
		copyValue := *value
		copyValue.Literal = reader
		return &copyValue, nil
	case *imap.FetchDataBodySection:
		copyValue := *value
		copyValue.Literal = reader
		return &copyValue, nil
	case *imap.FetchDataBinarySection:
		copyValue := *value
		copyValue.Literal = reader
		return &copyValue, nil
	default:
		return nil, fmt.Errorf("FETCH value %T is not a literal", value)
	}
}

func stageFetchLiteral(reader io.Reader, limit int64) (io.Reader, int64, func(), error) {
	file, err := os.CreateTemp("", "go-imap-fetch-*")
	if err != nil {
		return nil, 0, func() {}, fmt.Errorf("stage FETCH literal: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	count, copyErr := io.Copy(file, io.LimitReader(reader, limit+1))
	if closer, ok := reader.(io.Closer); ok {
		_ = closer.Close()
	}
	if copyErr != nil {
		cleanup()
		return nil, 0, func() {}, fmt.Errorf("stage FETCH literal: %w", copyErr)
	}
	if count > limit {
		cleanup()
		return nil, 0, func() {}, commandLimitError("FETCH response byte limit exceeded")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, 0, func() {}, fmt.Errorf("rewind FETCH literal: %w", err)
	}
	return file, count, cleanup, nil
}

func encodeFlagList(flags []imap.Flag) (string, error) {
	var buf bytes.Buffer
	enc := imapwire.NewEncoder(&buf, &imapwire.EncoderOptions{ServerResponse: true})
	enc.List(len(flags), func(i int) { enc.Flag(string(flags[i])) })
	if err := enc.Flush(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func copyUIDArgs(data *imap.CopyData) (string, bool) {
	if data == nil || !data.HasUIDs || data.UIDValidity == 0 || data.SourceUIDs.IsEmpty() || data.DestinationUIDs.IsEmpty() || data.SourceUIDs.Dynamic() || data.DestinationUIDs.Dynamic() {
		return "", false
	}
	return fmt.Sprintf("%d %s %s", data.UIDValidity, data.SourceUIDs.String(), data.DestinationUIDs.String()), true
}
