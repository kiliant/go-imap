package imapclient

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// QuotaResourceName is a QUOTA resource type. It is a string-backed named type
// rather than an enumeration: RFC 9208 registers STORAGE, MESSAGE, MAILBOX and
// ANNOTATION-STORAGE, and capa-quota-res ("QUOTA=RES-*") lets servers advertise
// further names without an API change.
type QuotaResourceName string

// Quota resource names from RFC 9208 section 5.
const (
	QuotaResourceStorage           QuotaResourceName = "STORAGE"
	QuotaResourceMessage           QuotaResourceName = "MESSAGE"
	QuotaResourceMailbox           QuotaResourceName = "MAILBOX"
	QuotaResourceAnnotationStorage QuotaResourceName = "ANNOTATION-STORAGE"
)

// QuotaResource is one resource usage/limit pair from a QUOTA response.
// RFC 9208 section 4.2.
//
// Construct with keyed fields only; fields may be added in a future release.
type QuotaResource struct {
	Name  QuotaResourceName
	Usage uint64
	Limit uint64
	_     struct{}
}

// QuotaData is one untagged QUOTA response. RFC 9208 section 4.2.
//
// Construct with keyed fields only; fields may be added in a future release.
type QuotaData struct {
	Root      string
	Resources []QuotaResource
	_         struct{}
}

// QuotaRootData is the result of GETQUOTAROOT: the mailbox's quota roots and
// every QUOTA response the server sent with them. RFC 9208 section 4.1.2.
//
// Construct with keyed fields only; fields may be added in a future release.
type QuotaRootData struct {
	Mailbox string
	Roots   []string
	Quotas  []QuotaData
	_       struct{}
}

// SetQuotaOptions configures SETQUOTA. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type SetQuotaOptions struct {
	_ struct{}
}

// QuotaResourceLimit is one SETQUOTA resource limit. Usage is not sent on the
// wire for SETQUOTA.
//
// Construct with keyed fields only; fields may be added in a future release.
type QuotaResourceLimit struct {
	Name  QuotaResourceName
	Limit uint64
	_     struct{}
}

// GetQuota returns the resource usage and limits for quotaRoot.
// QUOTA, RFC 9208 section 4.1.1.
//
// It requires a capability whose name is "QUOTA" or starts with "QUOTA=".
func (c *Client) GetQuota(ctx context.Context, quotaRoot string, options *GetQuotaOptions) (*QuotaData, error) {
	_ = options
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETQUOTA requires a non-nil context"}
	}
	if !c.quotaAvailable() {
		return nil, capabilityError("GETQUOTA", "QUOTA")
	}
	data := &QuotaData{}
	var got bool
	cmd := c.beginCommand("GETQUOTA", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Astring(quotaRoot)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "QUOTA" {
			return false, nil
		}
		parsed, err := readQuotaResponse(resp.dec)
		if err != nil {
			return true, err
		}
		*data = *parsed
		got = true
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if !got {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETQUOTA completed without a QUOTA response"}
	}
	return data, nil
}

// GetQuotaOptions configures GETQUOTA. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type GetQuotaOptions struct {
	_ struct{}
}

// GetQuotaRoot returns the quota roots that apply to mailbox, together with
// their usage and limits. GETQUOTAROOT, RFC 9208 section 4.1.2.
func (c *Client) GetQuotaRoot(ctx context.Context, mailbox string, options *GetQuotaRootOptions) (*QuotaRootData, error) {
	_ = options
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETQUOTAROOT requires a non-nil context"}
	}
	if mailbox == "" {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETQUOTAROOT requires a mailbox"}
	}
	if !c.quotaAvailable() {
		return nil, capabilityError("GETQUOTAROOT", "QUOTA")
	}
	data := &QuotaRootData{Mailbox: mailbox}
	var gotRoot bool
	limit := c.maxUntaggedResponses()
	count := 0
	cmd := c.beginCommand("GETQUOTAROOT", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil {
			return false, nil
		}
		switch resp.name {
		case "QUOTAROOT":
			if err := countUntaggedResponse(&count, limit, "GETQUOTAROOT"); err != nil {
				return true, err
			}
			root, err := readQuotaRootResponse(resp.dec)
			if err != nil {
				return true, err
			}
			data.Mailbox = root.Mailbox
			data.Roots = root.Roots
			gotRoot = true
			return true, nil
		case "QUOTA":
			if err := countUntaggedResponse(&count, limit, "GETQUOTAROOT"); err != nil {
				return true, err
			}
			parsed, err := readQuotaResponse(resp.dec)
			if err != nil {
				return true, err
			}
			data.Quotas = append(data.Quotas, *parsed)
			return true, nil
		}
		return false, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if !gotRoot {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETQUOTAROOT completed without a QUOTAROOT response"}
	}
	return data, nil
}

// GetQuotaRootOptions configures GETQUOTAROOT. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type GetQuotaRootOptions struct {
	_ struct{}
}

// SetQuota installs resource limits on quotaRoot. SETQUOTA, RFC 9208
// section 4.1.3. It requires the QUOTASET capability.
//
// An empty or nil limits slice clears every limit on the root. A nil options
// pointer selects the defaults.
func (c *Client) SetQuota(ctx context.Context, quotaRoot string, limits []QuotaResourceLimit, options *SetQuotaOptions) (*QuotaData, error) {
	_ = options
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SETQUOTA requires a non-nil context"}
	}
	if !c.Supports("QUOTASET") {
		return nil, capabilityError("SETQUOTA", "QUOTASET")
	}
	for _, limit := range limits {
		if strings.TrimSpace(string(limit.Name)) == "" {
			return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SETQUOTA resource name must not be empty"}
		}
	}
	data := &QuotaData{}
	var got bool
	cmd := c.beginCommand("SETQUOTA", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Astring(quotaRoot).SP().List(len(limits), func(i int) {
			enc.Atom(string(limits[i].Name)).SP().Number64(int64(limits[i].Limit))
		})
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "QUOTA" {
			return false, nil
		}
		parsed, err := readQuotaResponse(resp.dec)
		if err != nil {
			return true, err
		}
		*data = *parsed
		got = true
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if !got {
		// RFC 9208 permits SETQUOTA to succeed without echoing QUOTA when the
		// new limits match what was already in place; return an empty root.
		return &QuotaData{Root: quotaRoot}, nil
	}
	return data, nil
}

// QuotaResources returns the resource names advertised by QUOTA=RES-*
// capabilities. The wire form is "QUOTA=RES-STORAGE", not a "="-separated
// parameter, so this does not go through [Client.CapabilityValues]. The
// returned slice is owned by the caller.
func (c *Client) QuotaResources() []QuotaResourceName {
	c.mu.Lock()
	defer c.mu.Unlock()
	const prefix = "QUOTA=RES-"
	out := make([]QuotaResourceName, 0)
	for capability := range c.caps {
		if strings.HasPrefix(capability, prefix) {
			out = append(out, QuotaResourceName(strings.TrimPrefix(capability, prefix)))
		}
	}
	return out
}

func (c *Client) quotaAvailable() bool {
	// RFC 9208: GETQUOTA/GETQUOTAROOT require the bare QUOTA capability.
	// QUOTA=RES-* only names resource types and does not imply the commands;
	// QUOTASET only covers SETQUOTA.
	return c.Supports("QUOTA")
}

func readQuotaResponse(dec *imapwire.Decoder) (*QuotaData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var root string
	if !dec.ExpectAstring(&root) {
		return nil, dec.Err()
	}
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	data := &QuotaData{Root: root}
	err := dec.ExpectList(func() error {
		var name string
		if !dec.ExpectAtom(&name) || !dec.ExpectSP() {
			return dec.Err()
		}
		var usage, limit int64
		if !dec.ExpectNumber64(&usage) || !dec.ExpectSP() || !dec.ExpectNumber64(&limit) {
			return dec.Err()
		}
		if usage < 0 || limit < 0 {
			return fmt.Errorf("QUOTA resource %q has a negative usage or limit", name)
		}
		data.Resources = append(data.Resources, QuotaResource{
			Name:  QuotaResourceName(strings.ToUpper(name)),
			Usage: uint64(usage),
			Limit: uint64(limit),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return data, nil
}

func readQuotaRootResponse(dec *imapwire.Decoder) (*QuotaRootData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var mailbox string
	if !dec.ExpectMailbox(&mailbox) {
		return nil, dec.Err()
	}
	data := &QuotaRootData{Mailbox: mailbox}
	for dec.SP() {
		var root string
		if !dec.ExpectAstring(&root) {
			return nil, dec.Err()
		}
		data.Roots = append(data.Roots, root)
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return data, nil
}

// parseQuotaResourcePair is retained for fuzz targets that drive the resource
// triple without a full response line.
func parseQuotaResourcePair(name, usage, limit string) (QuotaResource, error) {
	u, err := strconv.ParseUint(usage, 10, 64)
	if err != nil {
		return QuotaResource{}, fmt.Errorf("invalid QUOTA usage %q", usage)
	}
	l, err := strconv.ParseUint(limit, 10, 64)
	if err != nil {
		return QuotaResource{}, fmt.Errorf("invalid QUOTA limit %q", limit)
	}
	return QuotaResource{Name: QuotaResourceName(strings.ToUpper(name)), Usage: u, Limit: l}, nil
}
