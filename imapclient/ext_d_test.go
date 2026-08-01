package imapclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestGetQuotaAndQuotaRoot(t *testing.T) {
	c, server := extDDial(t, func(tag, line string) string {
		switch {
		case strings.Contains(line, "GETQUOTAROOT"):
			return "* QUOTAROOT INBOX \"\"\r\n" +
				"* QUOTA \"\" (STORAGE 10 512)\r\n" +
				tag + " OK done\r\n"
		case strings.Contains(line, "GETQUOTA"):
			return "* QUOTA \"\" (STORAGE 10 512 MESSAGE 1 100)\r\n" + tag + " OK done\r\n"
		case strings.Contains(line, "SETQUOTA"):
			return "* QUOTA \"\" (STORAGE 10 1024)\r\n" + tag + " OK done\r\n"
		default:
			return tag + " BAD unexpected\r\n"
		}
	})
	extDReady(c, []string{"IMAP4rev1", "QUOTA", "QUOTASET", "QUOTA=RES-STORAGE"}, nil, false)
	ctx := extDContext(t)

	got, err := c.GetQuota(ctx, "", nil)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if got.Root != "" || len(got.Resources) != 2 {
		t.Fatalf("GetQuota = %#v", got)
	}
	if !strings.HasSuffix(server.LastLine(), `GETQUOTA ""`) && !strings.Contains(server.LastLine(), "GETQUOTA") {
		t.Fatalf("wire = %q", server.LastLine())
	}

	root, err := c.GetQuotaRoot(ctx, "INBOX", nil)
	if err != nil {
		t.Fatalf("GetQuotaRoot: %v", err)
	}
	if root.Mailbox != "INBOX" || len(root.Roots) != 1 || len(root.Quotas) != 1 {
		t.Fatalf("GetQuotaRoot = %#v", root)
	}

	set, err := c.SetQuota(ctx, "", []QuotaResourceLimit{{Name: QuotaResourceStorage, Limit: 1024}}, nil)
	if err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	if len(set.Resources) != 1 || set.Resources[0].Limit != 1024 {
		t.Fatalf("SetQuota = %#v", set)
	}

	res := c.QuotaResources()
	if len(res) != 1 || res[0] != QuotaResourceStorage {
		t.Fatalf("QuotaResources = %#v", res)
	}
}

func TestGetQuotaRequiresCapability(t *testing.T) {
	c, _ := extDDial(t, func(tag, _ string) string { return tag + " OK\r\n" })
	extDReady(c, []string{"IMAP4rev1"}, nil, false)
	_, err := c.GetQuota(extDContext(t), "", nil)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
}

func TestACLCommands(t *testing.T) {
	c, server := extDDial(t, func(tag, line string) string {
		switch {
		case strings.Contains(line, "GETACL"):
			return "* ACL INBOX user lrswipkxtecda anyone lrs\r\n" + tag + " OK done\r\n"
		case strings.Contains(line, "LISTRIGHTS"):
			return "* LISTRIGHTS INBOX user lrsa k x te c d\r\n" + tag + " OK done\r\n"
		case strings.Contains(line, "MYRIGHTS"):
			return "* MYRIGHTS INBOX lrswipkxtecda\r\n" + tag + " OK done\r\n"
		case strings.Contains(line, "SETACL"), strings.Contains(line, "DELETEACL"):
			return tag + " OK done\r\n"
		default:
			return tag + " BAD unexpected\r\n"
		}
	})
	extDReady(c, []string{"IMAP4rev1", "ACL", "RIGHTS=texk"}, nil, false)
	ctx := extDContext(t)

	acl, err := c.GetACL(ctx, "INBOX", nil)
	if err != nil || len(acl.Entries) != 2 {
		t.Fatalf("GetACL = %#v, %v", acl, err)
	}
	if err := c.SetACL(ctx, "INBOX", "user", "lrsw", nil); err != nil {
		t.Fatalf("SetACL: %v", err)
	}
	if !strings.Contains(server.LastLine(), "SETACL INBOX user lrsw") {
		t.Fatalf("wire = %q", server.LastLine())
	}
	if err := c.DeleteACL(ctx, "INBOX", "anyone", nil); err != nil {
		t.Fatalf("DeleteACL: %v", err)
	}
	lr, err := c.ListRights(ctx, "INBOX", "user", nil)
	if err != nil || lr.Required != "lrsa" || len(lr.Optional) != 5 {
		t.Fatalf("ListRights = %#v, %v", lr, err)
	}
	mr, err := c.MyRights(ctx, "INBOX", nil)
	if err != nil || mr.Rights != "lrswipkxtecda" {
		t.Fatalf("MyRights = %#v, %v", mr, err)
	}
	if got := c.RightsSets(); len(got) != 1 || !strings.EqualFold(got[0], "texk") {
		t.Fatalf("RightsSets = %#v", got)
	}
}

func TestMetadataCommands(t *testing.T) {
	c, server := extDDial(t, func(tag, line string) string {
		switch {
		case strings.Contains(line, "GETMETADATA"):
			return "* METADATA INBOX (/shared/comment \"Hello\" /private/comment NIL)\r\n" + tag + " OK done\r\n"
		case strings.Contains(line, "SETMETADATA"):
			return tag + " OK done\r\n"
		default:
			return tag + " BAD unexpected\r\n"
		}
	})
	extDReady(c, []string{"IMAP4rev1", "METADATA", "METADATA-SERVER"}, nil, false)
	ctx := extDContext(t)

	got, err := c.GetMetadata(ctx, "INBOX", []MetadataEntryName{"/shared/comment", "/private/comment"}, nil)
	if err != nil || len(got.Entries) != 2 || got.Entries[0].Value == nil || got.Entries[1].Value != nil {
		t.Fatalf("GetMetadata = %#v, %v", got, err)
	}
	err = c.SetMetadata(ctx, "INBOX", []MetadataEntry{{Name: "/shared/comment", Value: MetadataString("Hi")}}, nil)
	if err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if !strings.Contains(server.LastLine(), "SETMETADATA") {
		t.Fatalf("wire = %q", server.LastLine())
	}
}

func TestNotifyAndUnauthenticate(t *testing.T) {
	c, server := extDDial(t, func(tag, line string) string {
		switch {
		case strings.Contains(line, "NOTIFY"):
			return tag + " OK done\r\n"
		case strings.Contains(line, "UNAUTHENTICATE"):
			return tag + " OK [CAPABILITY IMAP4rev1 AUTH=PLAIN UNAUTHENTICATE] done\r\n"
		case strings.Contains(line, "CAPABILITY"):
			return "* CAPABILITY IMAP4rev1 AUTH=PLAIN UNAUTHENTICATE\r\n" + tag + " OK done\r\n"
		default:
			return tag + " BAD unexpected\r\n"
		}
	})
	extDReady(c, []string{"IMAP4rev1", "NOTIFY", "UNAUTHENTICATE"}, nil, true)
	ctx := extDContext(t)

	err := c.Notify(ctx, []NotifyFilter{{
		Specifier: NotifySelected,
		Events:    []NotifyEventName{NotifyEventMessageNew, NotifyEventMessageExpunge},
	}}, &NotifyOptions{Status: true})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(server.LastLine(), "NOTIFY STATUS SET") {
		t.Fatalf("wire = %q", server.LastLine())
	}
	if err := c.Notify(ctx, nil, &NotifyOptions{None: true}); err != nil {
		t.Fatalf("Notify NONE: %v", err)
	}
	if err := c.Unauthenticate(ctx, nil); err != nil {
		t.Fatalf("Unauthenticate: %v", err)
	}
	if c.State() != StateNotAuthenticated {
		t.Fatalf("state = %v", c.State())
	}
	if !c.Supports("IMAP4rev1") || !c.Supports("UNAUTHENTICATE") {
		t.Fatalf("caps after UNAUTHENTICATE = %#v", c.Capabilities())
	}
}

func TestUIDOnlyEnabled(t *testing.T) {
	c, _ := extDDial(t, func(tag, _ string) string { return tag + " OK\r\n" })
	extDReady(c, []string{"IMAP4rev1", "UIDONLY"}, []string{"UIDONLY"}, true)
	if !c.UIDOnlyEnabled() || !c.requireUIDCommands() {
		t.Fatal("expected UIDONLY enabled")
	}
	err := uidRequiredError("FETCH")
	if err.Code != imap.CodeUIDRequired {
		t.Fatalf("code = %v", err.Code)
	}
}

func TestInProgressParse(t *testing.T) {
	cases := []struct {
		args    string
		wantTag string
		prog    uint32
		hasP    bool
		goal    uint32
		hasG    bool
		fail    bool
	}{
		{"", "", 0, false, 0, false, false},
		{`("A001" NIL NIL)`, "A001", 0, false, 0, false, false},
		{`("A001" 175 NIL)`, "A001", 175, true, 0, false, false},
		{`("A001" 175 1000)`, "A001", 175, true, 1000, true, false},
		{`(NIL 1 2)`, "", 1, true, 2, true, false},
		{`("A001" 1000 1000)`, "", 0, false, 0, false, true},
		{`("A001" 5 0)`, "", 0, false, 0, false, true},
	}
	for _, tc := range cases {
		got, err := ParseInProgressArgs(tc.args)
		if tc.fail {
			if err == nil {
				t.Fatalf("ParseInProgressArgs(%q) = %#v, want error", tc.args, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseInProgressArgs(%q): %v", tc.args, err)
		}
		if got.Tag != tc.wantTag || got.HasProgress != tc.hasP || got.Progress != tc.prog || got.HasGoal != tc.hasG || got.Goal != tc.goal {
			t.Fatalf("ParseInProgressArgs(%q) = %#v", tc.args, got)
		}
	}
	c, _ := extDDial(t, func(tag, _ string) string { return tag + " OK\r\n" })
	extDReady(c, []string{"INPROGRESS"}, nil, false)
	if !c.SupportsInProgress() {
		t.Fatal("expected INPROGRESS")
	}
}

func TestMessageLimitAndJMAPAccess(t *testing.T) {
	c, _ := extDDial(t, func(tag, line string) string {
		if strings.Contains(line, "GETJMAPACCESS") {
			return `* JMAPACCESS "https://jmap.example/session"` + "\r\n" + tag + " OK done\r\n"
		}
		return tag + " BAD unexpected\r\n"
	})
	extDReady(c, []string{"IMAP4rev1", "MESSAGELIMIT=1000", "SAVELIMIT=500", "JMAPACCESS"}, nil, false)

	ml, err := c.MessageLimit()
	if err != nil || ml.Limit != 1000 || ml.SaveOnly {
		t.Fatalf("MessageLimit = %#v, %v", ml, err)
	}
	sl, err := c.SaveLimit()
	if err != nil || sl.Limit != 500 || !sl.SaveOnly {
		t.Fatalf("SaveLimit = %#v, %v", sl, err)
	}
	partial, err := ParseMessageLimitArgs("1000 23221")
	if err != nil || !partial.HasLowestUID || partial.LowestUID != 23221 {
		t.Fatalf("ParseMessageLimitArgs = %#v, %v", partial, err)
	}

	jmap, err := c.GetJMAPAccess(extDContext(t), nil)
	if err != nil || jmap.SessionURL != "https://jmap.example/session" {
		t.Fatalf("GetJMAPAccess = %#v, %v", jmap, err)
	}
}

func TestListMyRightsAndMetadata(t *testing.T) {
	c, _ := extDDial(t, func(tag, line string) string {
		if !strings.Contains(line, "LIST") {
			return tag + " BAD unexpected\r\n"
		}
		if strings.Contains(line, "MYRIGHTS") {
			return "* LIST () \"/\" INBOX\r\n" +
				"* MYRIGHTS INBOX lrswipkxtecda\r\n" +
				tag + " OK done\r\n"
		}
		return "* LIST () \"/\" INBOX\r\n" +
			"* METADATA INBOX (/shared/comment \"Hi\")\r\n" +
			tag + " OK done\r\n"
	})
	extDReady(c, []string{"IMAP4rev1", "LIST-EXTENDED", "LIST-MYRIGHTS", "LIST-METADATA"}, nil, false)
	ctx := extDContext(t)

	boxes, rights, err := c.ListMailboxesWithMyRights(ctx, "", "*", nil)
	if err != nil || len(boxes) != 1 || len(rights) != 1 || rights[0].Rights != "lrswipkxtecda" {
		t.Fatalf("ListMailboxesWithMyRights = %#v %#v %v", boxes, rights, err)
	}
	var meta []*MailboxMetadata
	boxes, err = c.ListMailboxesExt(ctx, "", "*", nil, &ListExtOptions{
		Metadata: &ListReturnMetadata{
			Entries: []MetadataEntryName{"/shared/comment"},
			Handler: func(d *MailboxMetadata) { meta = append(meta, d) },
		},
	})
	if err != nil || len(boxes) != 1 || len(meta) != 1 || meta[0].Entries[0].Value == nil {
		t.Fatalf("ListMailboxesExt METADATA = %#v %#v %v", boxes, meta, err)
	}
}
