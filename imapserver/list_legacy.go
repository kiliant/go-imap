package imapserver

import (
	"context"
	"github.com/kiliant/go-imap"
	"slices"
	"strings"
)

// RFC 3501 6.3.9: a trailing % must also expose hierarchy levels that
// contain subscribed descendants, even when those levels are not subscribed.
// The synthetic level is always marked Noselect in LSUB.
func listLegacySubscribed(ctx context.Context, session Session, writer *ListWriter, args *listArgs) error {
	if len(args.patterns) != 1 || args.patterns[0] == "" {
		return nil
	}
	all, err := collectList(ctx, session, "", []string{"*"}, &ListOptions{Subscribed: true})
	if err != nil {
		return err
	}
	results := make(map[string]*imap.ListData)
	for name, data := range all {
		if listPatternMatches(name, args.reference, args.patterns, data.Delimiter) {
			copyData := *data
			copyData.Attrs = nil
			if imap.ContainsAttr(data.Attrs, imap.MailboxAttrNoSelect) || imap.ContainsAttr(data.Attrs, imap.MailboxAttrNonExistent) {
				copyData.Attrs = []imap.MailboxAttr{imap.MailboxAttrNoSelect}
			}
			results[name] = &copyData
		}
		if strings.HasSuffix(args.patterns[0], "%") {
			for _, parent := range listParents(name, data.Delimiter) {
				if results[parent] == nil && listPatternMatches(parent, args.reference, args.patterns, data.Delimiter) {
					results[parent] = &imap.ListData{Mailbox: parent, Delimiter: data.Delimiter, Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}}
				}
			}
		}
	}
	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := writer.WriteList(ctx, results[name]); err != nil {
			return err
		}
	}
	return nil
}
