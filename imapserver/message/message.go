// Package message analyses stored RFC 5322 messages for IMAP backends. It
// produces ENVELOPE and BODYSTRUCTURE, streams exact BODY sections, and offers
// a portable SEARCH evaluator. Indexed backends can evaluate SEARCH themselves.
//
// Sources remain owned by the caller: keep their bytes immutable and their
// ReaderAt open until all operations and section readers finish. Message does
// not close a source or retain a complete copy of its bytes. Analysis scans
// bodies to locate MIME boundaries and count lines with bounded read buffers;
// retained state consists of bounded headers and MIME part metadata.
//
// Concurrent operations and independent section readers are safe when the
// source supports concurrent ReadAt calls. Metadata, criteria and options must
// not change during an operation. Each section reader is used by one goroutine
// at a time. Accessors return independent copies that callers may modify.
//
// Blocking calls require a non-nil context. Cancellation is checked between
// source reads and criteria; it cannot interrupt a source already blocked in
// ReadAt. No background goroutine is started. Cancelling Analyze's context
// after it returns does not cancel later operations on the Message.
package message

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapmessage"
)

// Message is an immutable analysed view over caller-owned message bytes.
// Construct it with [Analyze]; its zero value is not an analysed message.
type Message struct {
	parsed *imapmessage.Message
	source io.ReaderAt
}

// AnalyzeOptions configures [Analyze]. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type AnalyzeOptions struct {
	_ struct{}
}

// Analyze examines exactly size octets from source. A nil source, negative size
// or nil context is invalid; size zero represents an empty message. A source
// shorter than size produces an I/O error. Malformed MIME and header syntax are
// interpreted best-effort, with bounded nesting/parts/header retention, rather
// than rejected. Message bytes and BODY sections are never normalised.
// A nil options pointer selects the defaults.
func Analyze(ctx context.Context, source io.ReaderAt, size int64, options *AnalyzeOptions) (*Message, error) {
	_ = options
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if source == nil || size < 0 {
		return nil, fmt.Errorf("message: invalid message source")
	}
	reader := &operationSource{ctx: ctx, source: source}
	if size > 0 {
		// Opaque bodies need not be scanned. Verify their declared end with a
		// one-byte probe so a short source cannot silently acquire a false size.
		var last [1]byte
		_, err := reader.ReadAt(last[:], size-1)
		if err != nil && err != io.EOF {
			return nil, operationError(ctx, reader, err)
		}
	}
	parsed, err := imapmessage.Analyze(reader, size)
	if err := operationError(ctx, reader, err); err != nil {
		return nil, err
	}
	return &Message{parsed: parsed.WithReader(source), source: source}, nil
}

// Size returns the source's declared octet count, or zero for a nil/zero Message.
func (m *Message) Size() int64 {
	if m == nil || m.parsed == nil {
		return 0
	}
	return m.parsed.Size()
}

// Envelope returns an independent deep copy, or nil for a nil/zero Message.
// It performs no I/O.
func (m *Message) Envelope() *imap.Envelope {
	if m == nil || m.parsed == nil {
		return nil
	}
	return cloneEnvelope(m.parsed.Envelope)
}

// BodyStructure returns an independent deep copy, or nil for a nil/zero Message.
// It performs no I/O.
func (m *Message) BodyStructure() imap.BodyStructure {
	if m == nil || m.parsed == nil {
		return nil
	}
	return cloneStructure(m.parsed.BodyStructure)
}

// SectionOptions configures [Message.OpenBodySection]. A nil pointer selects
// the defaults: return the requested stored bytes without transformation.
// Empty today; helper policy belongs here rather than in shared wire vocabulary.
// Construct with keyed fields only; fields may be added in a future release.
type SectionOptions struct{ _ struct{} }

// OpenBodySection returns a streaming reader and its exact octet count. item
// selects a part, HEADER/TEXT/MIME, header-field filters and optional partial.
// It must be non-nil; an empty item selects the complete stored message.
// Peek has no effect: this utility never changes mailbox flags.
// A nil options pointer selects the defaults.
//
// Missing parts and invalid requests return errors. Offsets beyond the section
// produce an empty reader. The reader is bound to ctx for its entire lifetime,
// owns no source copy, and needs no Close. Keep the source open and immutable
// until reading finishes; other Message operations and readers remain usable
// if this reader's context is cancelled. A nil/zero Message is invalid.
func (m *Message) OpenBodySection(ctx context.Context, item *imap.FetchItemBodySection, options *SectionOptions) (io.Reader, int64, error) {
	_ = options
	if err := checkContext(ctx); err != nil {
		return nil, 0, err
	}
	if m == nil || m.parsed == nil {
		return nil, 0, fmt.Errorf("message: nil or unanalysed message")
	}
	source := &operationSource{ctx: ctx, source: m.source}
	reader, size, err := m.parsed.WithReader(source).OpenBodySection(item)
	if err := operationError(ctx, source, err); err != nil {
		return nil, 0, err
	}
	return &sectionReader{ctx: ctx, reader: reader}, size, nil
}

// BinarySectionOptions configures [Message.OpenBinarySection]. A nil pointer
// selects the defaults. Construct with keyed fields only; fields may be added.
type BinarySectionOptions struct{ _ struct{} }

// OpenBinarySection returns a section's decoded bytes and exact octet count.
// It supports identity, base64 and quoted-printable transfer encodings; text
// line endings become CRLF. Base64 decoding ignores characters outside the
// alphabet, as RFC 2045 section 6.8 requires. Parts follow BODY numbering, with
// an empty path selecting the entire message, retaining its headers. A
// multipart or encapsulated message container is decoded by its own transfer
// encoding, which RFC 2046 limits to identity encodings, so its stored octets
// are returned unchanged (RFC 3516; RFC 9051 lets an IMAP4rev2 client ask only
// for leaf parts).
//
// Partial ranges apply after decoding and line-ending conversion. Peek has no
// effect on this utility. Nil items and invalid ranges fail; unknown transfer
// encodings fail with *imap.Error carrying CodeUnknownCTE.
//
// Computing the exact count scans the decoded body with bounded buffers, then
// reopens it for streaming; no complete body copy is retained. The returned
// reader has the same context and source-lifetime guarantees as OpenBodySection.
// A nil options pointer selects the defaults.
func (m *Message) OpenBinarySection(ctx context.Context, item *imap.FetchItemBinarySection, options *BinarySectionOptions) (io.Reader, int64, error) {
	_ = options
	if err := checkContext(ctx); err != nil {
		return nil, 0, err
	}
	if m == nil || m.parsed == nil {
		return nil, 0, fmt.Errorf("message: nil or unanalysed message")
	}
	source := &operationSource{ctx: ctx, source: m.source}
	reader, size, err := m.parsed.WithReader(source).OpenBinarySection(item)
	if err := operationError(ctx, source, err); err != nil {
		return nil, 0, err
	}
	return &sectionReader{ctx: ctx, reader: reader}, size, nil
}

// EncodeOptions configures [Message.EncodeNULParts]. A nil pointer selects the
// defaults. Construct with keyed fields only; fields may be added.
type EncodeOptions struct{ _ struct{} }

// EncodeNULParts writes the message to w with every leaf part whose stored body
// holds a NUL octet under an identity transfer encoding (none, 7bit, 8bit or
// binary) re-encoded as base64. Every other byte is copied unchanged, header
// lines other than Content-Transfer-Encoding included. It reports whether any
// part changed; when none did, it writes nothing and the original bytes remain
// the message to store.
//
// No IMAP string can carry NUL, so BODY cannot transmit such a part. A backend
// that accepts BINARY APPEND (RFC 3516) can store the result instead: section
// 4.4 permits the change because nothing is lost, BODY and BODYSTRUCTURE then
// describe base64, and [Message.OpenBinarySection] still returns the original
// octets. The message is read once to find such parts and once to write it,
// with bounded buffers. A nil options pointer selects the defaults.
func (m *Message) EncodeNULParts(ctx context.Context, w io.Writer, options *EncodeOptions) (bool, error) {
	_ = options
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	if m == nil || m.parsed == nil || w == nil {
		return false, fmt.Errorf("message: nil or unanalysed message, or nil writer")
	}
	source := &operationSource{ctx: ctx, source: m.source}
	sink := &operationSink{writer: w}
	changed, err := m.parsed.WithReader(source).EncodeNULParts(sink)
	if sink.err != nil && ctx.Err() == nil {
		return changed, sink.err // the caller's own writer error, unchanged
	}
	return changed, operationError(ctx, source, err)
}

type operationSink struct {
	writer io.Writer
	err    error
}

func (w *operationSink) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

type operationSource struct {
	ctx    context.Context
	source io.ReaderAt
	err    error
}

func (r *operationSource) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	n, err := r.source.ReadAt(p, offset)
	if n < len(p) && (err == nil || err == io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

type sectionReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *sectionReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("message: a non-nil context is required")
	}
	return ctx.Err()
}

func operationError(ctx context.Context, source *operationSource, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if source.err != nil {
		return source.err // retain the caller's original I/O error identity
	}
	var protocolErr *imap.Error
	if errors.As(err, &protocolErr) {
		return protocolErr
	}
	if errors.Is(err, imapmessage.ErrUnsupportedCriteria) {
		return fmt.Errorf("%w%s", ErrUnsupportedCriteria, strings.TrimPrefix(err.Error(), imapmessage.ErrUnsupportedCriteria.Error()))
	}
	if err != nil {
		// Parser implementation errors and sentinels are deliberately not
		// exposed through Unwrap. Public errors use the supported vocabulary.
		return fmt.Errorf("message: %s", strings.TrimPrefix(err.Error(), "imapmessage: "))
	}
	return nil
}
