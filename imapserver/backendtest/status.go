package backendtest

import (
	"context"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func runStatusWitnesses(t *testing.T, harness *Harness) {
	t.Run("status-deleted-witness", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		witness, ok := backendWitness(instance, session)
		if !ok || !witness.SupportsCapability(t.Context(), "STATUS-DELETED", nil) {
			t.Skip("no STATUS-DELETED witness")
		}
		checkDeletedStatusItems(t, session, true, false)
	})
	t.Run("quota-resource-witnesses", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "QUOTA")
		quota, ok := session.(imapserver.QuotaSession)
		if !ok {
			t.Skip("no QuotaSession")
		}
		witness, ok := backendWitness(instance, session)
		if !ok {
			t.Skip("RFC 2087 QUOTA without resource declarations")
		}
		resources := []string{"STORAGE", "MESSAGE", "MAILBOX", "ANNOTATION-STORAGE"}
		declared := make(map[string]bool)
		for _, resource := range resources {
			declared[resource] = witness.SupportsCapability(t.Context(), "QUOTA=RES-"+resource, nil)
		}
		modern := false
		for _, supported := range declared {
			modern = modern || supported
		}
		if !modern {
			t.Skip("RFC 2087 QUOTA without resource declarations")
		}
		mailbox := populate(t, session, "quota-resources")
		roots, err := quota.QuotaRoots(t.Context(), mailbox, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, root := range roots {
			data, err := quota.GetQuota(t.Context(), root, nil)
			if err != nil || data == nil {
				t.Fatalf("GetQuota(%s) = %v, %v", root, data, err)
			}
			for _, resource := range data.Resources {
				if !witness.SupportsCapability(t.Context(), "QUOTA=RES-"+string(resource.Name), nil) {
					t.Errorf("quota resource %s has no witness", resource.Name)
				}
			}
		}
		// Resource witnesses are promises even if wire capabilities are filtered:
		// filtering controls advertisement, not the backend's declared semantics.
		if declared["STORAGE"] || declared["MESSAGE"] {
			checkDeletedStatusItems(t, session, declared["MESSAGE"], declared["STORAGE"])
		}
	})
	t.Run("objectid-stable-mailbox-id", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "OBJECTID")
		witness, ok := backendWitness(instance, session)
		if !ok || !witness.SupportsCapability(t.Context(), "OBJECTID", nil) {
			t.Skip("no OBJECTID witness")
		}
		mailbox := populate(t, session, "objectid")
		read := func() string {
			t.Helper()
			data, err := session.Status(t.Context(), mailbox, &imapserver.StatusOptions{Items: []imap.StatusItem{imap.StatusItemMailboxID}})
			if err != nil || data == nil {
				t.Fatalf("STATUS MAILBOXID = %v, %v", data, err)
			}
			id, _ := data.Values[imap.StatusItemMailboxID].(string)
			if id == "" {
				t.Fatal("missing MAILBOXID")
			}
			return id
		}
		before := read()
		selection := selectMailbox(t, session, mailbox, discardUpdater())
		defer selection.Mailbox.Unselect(context.Background(), nil)
		if after := read(); before != after {
			t.Errorf("MAILBOXID changed on SELECT: %s -> %s", before, after)
		}
	})
}

func checkDeletedStatusItems(t *testing.T, session imapserver.Session, messages, storage bool) {
	t.Helper()
	const mailbox = "backendtest-deleted-status"
	if err := session.Create(t.Context(), mailbox, nil); err != nil {
		t.Fatal(err)
	}
	// Supply real deleted content while leaving a live message in the mailbox.
	raw := "Subject: deleted\r\n\r\n" + strings.Repeat("x", 1200)
	for _, flags := range [][]imap.Flag{{imap.FlagDeleted}, nil} {
		if _, err := session.Append(t.Context(), mailbox, strings.NewReader(raw), &imapserver.AppendOptions{Flags: flags}); err != nil {
			t.Fatal(err)
		}
	}
	items := []imap.StatusItem{}
	if messages {
		items = append(items, imap.StatusItemDeleted)
	}
	if storage {
		items = append(items, imap.StatusItemDeletedStorage)
	}
	check := func() {
		t.Helper()
		data, err := session.Status(t.Context(), mailbox, &imapserver.StatusOptions{Items: items})
		if err != nil || data == nil {
			t.Fatalf("deleted STATUS = %v, %v", data, err)
		}
		if messages {
			if n, ok := data.Number(imap.StatusItemDeleted); !ok || n != 1 {
				t.Errorf("DELETED = %d, present %t, want 1", n, ok)
			}
		}
		// Physical accounting may include metadata or compression; the witness
		// requires a numeric estimate, not a prescribed storage layout.
		if storage {
			if n, ok := data.Number(imap.StatusItemDeletedStorage); !ok {
				t.Errorf("DELETED-STORAGE = %d, present %t", n, ok)
			}
		}
	}
	check() // Unselected mailbox support is the reason for the witness.
	selected := selectMailbox(t, session, mailbox, discardUpdater())
	defer selected.Mailbox.Unselect(context.Background(), nil)
	check()
}
