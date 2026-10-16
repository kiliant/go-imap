package message

import (
	"maps"
	"slices"

	"github.com/kiliant/go-imap"
)

func cloneEnvelope(source *imap.Envelope) *imap.Envelope {
	if source == nil {
		return nil
	}
	copy := *source
	copy.From = slices.Clone(source.From)
	copy.Sender = slices.Clone(source.Sender)
	copy.ReplyTo = slices.Clone(source.ReplyTo)
	copy.To = slices.Clone(source.To)
	copy.Cc = slices.Clone(source.Cc)
	copy.Bcc = slices.Clone(source.Bcc)
	copy.InReplyTo = slices.Clone(source.InReplyTo)
	return &copy
}

func cloneDisposition(source *imap.BodyStructureDisposition) *imap.BodyStructureDisposition {
	if source == nil {
		return nil
	}
	copy := *source
	copy.Params = maps.Clone(source.Params)
	return &copy
}

func cloneStructure(source imap.BodyStructure) imap.BodyStructure {
	switch source := source.(type) {
	case *imap.BodyStructureSinglePart:
		copy := *source
		copy.Params = maps.Clone(source.Params)
		if source.Text != nil {
			text := *source.Text
			copy.Text = &text
		}
		if source.Message != nil {
			message := *source.Message
			message.Envelope = cloneEnvelope(source.Message.Envelope)
			message.BodyStructure = cloneStructure(source.Message.BodyStructure)
			copy.Message = &message
		}
		if source.Extended != nil {
			extended := *source.Extended
			extended.Disp = cloneDisposition(source.Extended.Disp)
			extended.Lang = slices.Clone(source.Extended.Lang)
			copy.Extended = &extended
		}
		return &copy
	case *imap.BodyStructureMultiPart:
		copy := *source
		copy.Children = make([]imap.BodyStructure, len(source.Children))
		for i, child := range source.Children {
			copy.Children[i] = cloneStructure(child)
		}
		if source.Extended != nil {
			extended := *source.Extended
			extended.Params = maps.Clone(source.Extended.Params)
			extended.Disp = cloneDisposition(source.Extended.Disp)
			extended.Lang = slices.Clone(source.Extended.Lang)
			copy.Extended = &extended
		}
		return &copy
	default:
		// The analyser currently produces only the two shapes above. The
		// shape-coverage test requires additions to acquire a deep copy here.
		return nil
	}
}
