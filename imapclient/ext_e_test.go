package imapclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestReferralParse(t *testing.T) {
	got, err := ParseReferralArgs("IMAP://user;AUTH=*@SERVER2/")
	if err != nil || got.URL != "IMAP://user;AUTH=*@SERVER2/" {
		t.Fatalf("got %#v err %v", got, err)
	}
	got, err = ParseReferralArgs(`"IMAP://user@host/"`)
	if err != nil || got.URL != "IMAP://user@host/" {
		t.Fatalf("quoted %#v err %v", got, err)
	}
	ref, ok := ReferralFromError(&imap.Error{Code: imap.CodeReferral, CodeArgs: "IMAP://x/"})
	if !ok || ref.URL != "IMAP://x/" {
		t.Fatalf("ReferralFromError = %#v %v", ref, ok)
	}
	c, _ := extEDial(t, func(tag, _ string) string { return tag + " OK\r\n" })
	extEReady(c, []string{"LOGIN-REFERRALS", "MAILBOX-REFERRALS"}, nil, false)
	if !c.SupportsLoginReferrals() || !c.SupportsMailboxReferrals() {
		t.Fatal("expected referral caps")
	}
}

func TestURLAuthCommands(t *testing.T) {
	c, server := extEDial(t, func(tag, line string) string {
		switch {
		case strings.Contains(line, "GENURLAUTH"):
			return `* GENURLAUTH "imap://joe@example.com/INBOX/;uid=20/;section=1.2;urlauth=anonymous:internal:91354a473744909de610943775f92055"` + "\r\n" + tag + " OK done\r\n"
		case strings.Contains(line, "URLFETCH"):
			return `* URLFETCH "imap://joe@example.com/INBOX/;uid=20/;section=1.2;urlauth=anonymous:internal:91354a473744909de610943775f92055" "body"` + "\r\n" + tag + " OK done\r\n"
		case strings.Contains(line, "RESETKEY"):
			return tag + " OK done\r\n"
		default:
			return tag + " BAD unexpected\r\n"
		}
	})
	extEReady(c, []string{"IMAP4rev1", "URLAUTH", "URL-PARTIAL"}, nil, false)
	ctx := extEContext(t)

	gen, err := c.GenURLAuth(ctx, []GenURLAuthRequest{{
		RumpURL: "imap://joe@example.com/INBOX/;uid=20/;section=1.2;urlauth=anonymous",
	}}, nil)
	if err != nil || len(gen.URLs) != 1 {
		t.Fatalf("GenURLAuth = %#v %v", gen, err)
	}
	items, err := c.URLFetch(ctx, gen.URLs, nil)
	if err != nil || len(items) != 1 || items[0].Body == nil || *items[0].Body != "body" {
		t.Fatalf("URLFetch = %#v %v", items, err)
	}
	if err := c.ResetKey(ctx, nil); err != nil {
		t.Fatalf("ResetKey: %v", err)
	}
	if err := c.ResetKey(ctx, &ResetKeyOptions{Mailbox: "INBOX", Mechanisms: []URLAuthMechanism{URLAuthInternal}}); err != nil {
		t.Fatalf("ResetKey mailbox: %v", err)
	}
	if !strings.Contains(server.LastLine(), "RESETKEY INBOX INTERNAL") {
		t.Fatalf("wire = %q", server.LastLine())
	}
	if !c.SupportsURLPartial() {
		t.Fatal("expected URL-PARTIAL")
	}
}

func TestLanguageAndComparator(t *testing.T) {
	c, _ := extEDial(t, func(tag, line string) string {
		switch {
		case strings.Contains(line, "LANGUAGE"):
			if strings.HasSuffix(line, "LANGUAGE") {
				return `* LANGUAGE ("EN" "DE" "IT" "i-default")` + "\r\n" + tag + " OK done\r\n"
			}
			return `* LANGUAGE ("DE")` + "\r\n" + tag + " OK done\r\n"
		case strings.Contains(line, "COMPARATOR"):
			return `* COMPARATOR "i;basic" ("i;basic" "i;octet")` + "\r\n" + tag + " OK done\r\n"
		default:
			return tag + " BAD unexpected\r\n"
		}
	})
	extEReady(c, []string{"LANGUAGE", "I18NLEVEL=2"}, nil, false)
	ctx := extEContext(t)

	langs, err := c.Language(ctx, nil)
	if err != nil || len(langs.Tags) != 4 {
		t.Fatalf("Language enum = %#v %v", langs, err)
	}
	langs, err = c.Language(ctx, &LanguageOptions{Tags: []string{"DE"}})
	if err != nil || len(langs.Tags) != 1 || langs.Tags[0] != "DE" {
		t.Fatalf("Language set = %#v %v", langs, err)
	}
	cmp, err := c.Comparator(ctx, &ComparatorOptions{Wanted: []string{"i;basic"}})
	if err != nil || cmp.Active != "i;basic" || len(cmp.Matching) != 2 {
		t.Fatalf("Comparator = %#v %v", cmp, err)
	}
	if c.I18NLevel() != 2 {
		t.Fatalf("I18NLevel = %d", c.I18NLevel())
	}
}

func TestCancelUpdateAndNoUpdate(t *testing.T) {
	c, server := extEDial(t, func(tag, line string) string {
		if strings.Contains(line, "CANCELUPDATE") {
			return tag + " OK done\r\n"
		}
		return tag + " BAD unexpected\r\n"
	})
	extEReady(c, []string{"CONTEXT=SEARCH", "ESORT"}, nil, true)
	if err := c.CancelUpdate(extEContext(t), []string{"B01"}, nil); err != nil {
		t.Fatalf("CancelUpdate: %v", err)
	}
	if !strings.Contains(server.LastLine(), `CANCELUPDATE "B01"`) {
		t.Fatalf("wire = %q", server.LastLine())
	}
	tag, err := ParseNoUpdateArgs(`"B02"`)
	if err != nil || tag != "B02" {
		t.Fatalf("ParseNoUpdateArgs = %q %v", tag, err)
	}
	if !c.SupportsESort() || !c.SupportsContextSearch() {
		t.Fatal("expected context caps")
	}
}

func TestFiltersAndDeferredParse(t *testing.T) {
	name, err := ParseUndefinedFilterArgs("on-the-road")
	if err != nil || name != "on-the-road" {
		t.Fatalf("ParseUndefinedFilterArgs = %q %v", name, err)
	}
	got, ok := UndefinedFilterFromError(&imap.Error{Code: imap.CodeUndefinedFilter, CodeArgs: "missing"})
	if !ok || got != "missing" {
		t.Fatalf("UndefinedFilterFromError = %q %v", got, ok)
	}
	c, _ := extEDial(t, func(tag, _ string) string { return tag + " OK\r\n" })
	extEReady(c, []string{"FILTERS", "CONVERT", "ANNOTATE-EXPERIMENT-1", "IMAPSIEVE=vacation"}, nil, false)
	if !c.SupportsFilters() || !c.SupportsConvert() || !c.SupportsAnnotateExperiment() {
		t.Fatal("expected deferred/filter caps")
	}
	if scripts := c.IMAPSieveScripts(); len(scripts) != 1 || !strings.EqualFold(scripts[0], "vacation") {
		t.Fatalf("IMAPSieveScripts = %#v", scripts)
	}
	n, err := ParseMaxConvertMessagesArgs("10")
	if err != nil || n != 10 {
		t.Fatalf("ParseMaxConvertMessagesArgs = %d %v", n, err)
	}
	code, args, ok := ConvertFromError(&imap.Error{Code: imap.CodeTempFail, CodeArgs: "x"})
	if !ok || code != imap.CodeTempFail || args != "x" {
		t.Fatalf("ConvertFromError = %v %q %v", code, args, ok)
	}
	if _, err := ParseAnnotateArgs("TOOMANY"); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityAbsentFallbacks(t *testing.T) {
	c, _ := extEDial(t, func(tag, _ string) string { return tag + " OK\r\n" })
	extEReady(c, []string{"IMAP4rev1"}, nil, false)
	ctx := extEContext(t)
	if _, err := c.GenURLAuth(ctx, []GenURLAuthRequest{{RumpURL: "x"}}, nil); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("GenURLAuth err = %v", err)
	}
	if _, err := c.Language(ctx, nil); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("Language err = %v", err)
	}
	if err := c.CancelUpdate(ctx, []string{"A"}, nil); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("CancelUpdate err = %v", err)
	}
}
