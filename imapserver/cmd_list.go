package imapserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type listArgs struct {
	reference string
	patterns  []string
	selection []string
	legacy    bool
}

func parseList(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &listArgs{}
	if decoder.PeekSpecial('(') {
		if err := decoder.ExpectList(func() error {
			var option string
			if !decoder.ExpectAtom(&option) {
				return decoder.Err()
			}
			args.selection = append(args.selection, strings.ToUpper(option))
			return nil
		}); err != nil {
			return nil, 0, err
		}
		if !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	if !decoder.ExpectMailbox(&args.reference) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	if decoder.PeekSpecial('(') {
		if err := decoder.ExpectList(func() error {
			var pattern string
			if !decoder.ListMailbox(&pattern) {
				return decoder.Err()
			}
			args.patterns = append(args.patterns, pattern)
			return nil
		}); err != nil {
			return nil, 0, err
		}
	} else {
		var pattern string
		if !decoder.ListMailbox(&pattern) {
			return nil, 0, decoder.Err()
		}
		args.patterns = append(args.patterns, pattern)
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, listArgsSize(args), nil
}

func parseLsub(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &listArgs{legacy: true, selection: []string{"SUBSCRIBED"}}
	var pattern string
	if !decoder.ExpectMailbox(&args.reference) || !decoder.ExpectSP() || !decoder.ListMailbox(&pattern) || !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	args.patterns = []string{pattern}
	return args, listArgsSize(args), nil
}

func listArgsSize(args *listArgs) int64 {
	if args == nil {
		return 0
	}
	size := int64(len(args.reference))
	for _, value := range args.patterns {
		size += int64(len(value))
	}
	for _, value := range args.selection {
		size += int64(len(value))
	}
	return size
}

func handleList(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*listArgs)
	if args == nil || len(args.patterns) == 0 {
		return c.writeBad(command.tag, "invalid LIST arguments")
	}
	features := activeFeatures(&c.state, c.server)
	if len(args.patterns) > 1 && !features[featureListMulti] {
		return c.writeBad(command.tag, "multiple LIST patterns are not enabled")
	}
	options := &ListOptions{}
	for _, option := range args.selection {
		if option != "SUBSCRIBED" || !args.legacy && !features[featureListSubscribe] {
			return c.writeBad(command.tag, fmt.Sprintf("unsupported LIST selection option %q", option))
		}
		options.Subscribed = true
	}
	count := 0
	writtenName := "LIST"
	if args.legacy {
		writtenName = "LSUB"
	}
	writer := newListWriter(func(_ context.Context, data *imap.ListData) error {
		if data == nil || data.Mailbox == "" {
			return fmt.Errorf("imapserver: backend LIST returned an invalid mailbox")
		}
		count++
		if count > maxCommandListResults {
			return commandLimitError("LIST result limit exceeded")
		}
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom(writtenName).SP().List(len(data.Attrs), func(i int) {
			c.encoder.Flag(string(data.Attrs[i]))
		}).SP()
		if data.Delimiter == 0 {
			c.encoder.NIL()
		} else {
			c.encoder.String(string(data.Delimiter))
		}
		c.encoder.SP().Mailbox(data.Mailbox).CRLF()
		return c.encoder.Flush()
	})
	err := c.state.session.List(ctx, writer, args.reference, args.patterns, options)
	writer.core.close()
	if err != nil {
		return writeBackendError(c, command.tag, writtenName, err)
	}
	return c.writeTagged(command.tag, "OK", writtenName+" completed")
}
