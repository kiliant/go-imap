package imap

// QuotaResourceName is a QUOTA resource type. QUOTA, RFC 9208.
//
// It is a string-backed named type rather than an enumeration: RFC 9208
// registers STORAGE, MESSAGE, MAILBOX and ANNOTATION-STORAGE, and the
// capa-quota-res form ("QUOTA=RES-*") lets a server advertise further names
// without an API change.
type QuotaResourceName string

// Quota resource names from RFC 9208 section 5.
const (
	QuotaResourceStorage           QuotaResourceName = "STORAGE"
	QuotaResourceMessage           QuotaResourceName = "MESSAGE"
	QuotaResourceMailbox           QuotaResourceName = "MAILBOX"
	QuotaResourceAnnotationStorage QuotaResourceName = "ANNOTATION-STORAGE"
)

// QuotaResource is one resource usage/limit triple of a QUOTA response.
// RFC 9208 section 4.2.
//
// Construct with keyed fields only; fields may be added in a future release.
type QuotaResource struct {
	Name  QuotaResourceName
	Usage uint64
	Limit uint64
	_     struct{}
}

// QuotaData is the content of one untagged QUOTA response. RFC 9208
// section 4.2.
//
// Construct with keyed fields only; fields may be added in a future release.
type QuotaData struct {
	Root      string
	Resources []QuotaResource
	_         struct{}
}

// QuotaRootData pairs an untagged QUOTAROOT response with the QUOTA responses
// that accompany it. RFC 9208 section 4.1.2.
//
// Roots may be empty: RFC 9208 permits a mailbox with no quota root, and that
// is distinct from a root with no resources.
//
// Construct with keyed fields only; fields may be added in a future release.
type QuotaRootData struct {
	Mailbox string
	Roots   []string
	Quotas  []QuotaData
	_       struct{}
}

// QuotaResourceLimit is one resource limit of a SETQUOTA command. Usage is not
// carried on the wire for SETQUOTA, which is why this is a distinct type from
// [QuotaResource] rather than the same type with a field left zero. RFC 9208
// section 4.1.3.
//
// Construct with keyed fields only; fields may be added in a future release.
type QuotaResourceLimit struct {
	Name  QuotaResourceName
	Limit uint64
	_     struct{}
}
