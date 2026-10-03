package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// folderNode is one level in a mailbox hierarchy derived from LIST results.
type folderNode struct {
	name     string // segment at this level (not the full path)
	mailbox  string // full mailbox name when this node is a real LIST result
	attrs    []imap.MailboxAttr
	children map[string]*folderNode
}

func newFolderNode(name string) *folderNode {
	return &folderNode{
		name:     name,
		children: make(map[string]*folderNode),
	}
}

// buildFolderTree turns flat LIST data into a hierarchy using each mailbox's
// server-reported delimiter. A zero delimiter means the name is a single
// segment (RFC 3501 NIL hierarchy delimiter).
func buildFolderTree(mailboxes []*imapclient.ListData) *folderNode {
	root := newFolderNode("")
	for _, m := range mailboxes {
		if m == nil || m.Mailbox == "" {
			continue
		}
		insertMailbox(root, m)
	}
	return root
}

func insertMailbox(root *folderNode, m *imapclient.ListData) {
	segments := mailboxSegments(m.Mailbox, m.Delimiter)
	if len(segments) == 0 {
		return
	}
	node := root
	for i, seg := range segments {
		child, ok := node.children[seg]
		if !ok {
			child = newFolderNode(seg)
			node.children[seg] = child
		}
		if i == len(segments)-1 {
			child.mailbox = m.Mailbox
			child.attrs = append([]imap.MailboxAttr(nil), m.Attrs...)
		}
		node = child
	}
}

func mailboxSegments(name string, delim rune) []string {
	if delim == 0 || !strings.ContainsRune(name, delim) {
		return []string{name}
	}
	return strings.Split(name, string(delim))
}

// writeFolderTree prints the mailbox hierarchy to w.
func writeFolderTree(w io.Writer, root *folderNode) error {
	names := sortedChildNames(root)
	for i, name := range names {
		if err := writeFolderNode(w, root.children[name], "", i == len(names)-1); err != nil {
			return err
		}
	}
	return nil
}

func writeFolderNode(w io.Writer, node *folderNode, prefix string, last bool) error {
	branch := "├── "
	nextPrefix := prefix + "│   "
	if last {
		branch = "└── "
		nextPrefix = prefix + "    "
	}
	label := formatFolderLabel(node)
	if _, err := fmt.Fprintf(w, "%s%s%s\n", prefix, branch, label); err != nil {
		return err
	}
	names := sortedChildNames(node)
	for i, name := range names {
		if err := writeFolderNode(w, node.children[name], nextPrefix, i == len(names)-1); err != nil {
			return err
		}
	}
	return nil
}

func sortedChildNames(node *folderNode) []string {
	names := make([]string, 0, len(node.children))
	for name := range node.children {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func formatFolderLabel(node *folderNode) string {
	label := node.name
	if notes := folderNotes(node.attrs); notes != "" {
		label += "  [" + notes + "]"
	}
	return label
}

func folderNotes(attrs []imap.MailboxAttr) string {
	var notes []string
	if imap.ContainsAttr(attrs, imap.MailboxAttrNoSelect) {
		notes = append(notes, "noselect")
	}
	if imap.ContainsAttr(attrs, imap.MailboxAttrNonExistent) {
		notes = append(notes, "nonexistent")
	}
	for _, attr := range attrs {
		switch {
		case attr.Equal(imap.MailboxAttrAll):
			notes = append(notes, "\\All")
		case attr.Equal(imap.MailboxAttrArchive):
			notes = append(notes, "\\Archive")
		case attr.Equal(imap.MailboxAttrDrafts):
			notes = append(notes, "\\Drafts")
		case attr.Equal(imap.MailboxAttrFlagged):
			notes = append(notes, "\\Flagged")
		case attr.Equal(imap.MailboxAttrImportant):
			notes = append(notes, "\\Important")
		case attr.Equal(imap.MailboxAttrJunk):
			notes = append(notes, "\\Junk")
		case attr.Equal(imap.MailboxAttrSent):
			notes = append(notes, "\\Sent")
		case attr.Equal(imap.MailboxAttrTrash):
			notes = append(notes, "\\Trash")
		}
	}
	return strings.Join(notes, ", ")
}
