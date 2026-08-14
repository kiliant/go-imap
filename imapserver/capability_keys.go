package imapserver

import (
	"fmt"

	"github.com/kiliant/go-imap"
)

// Capability gating for search keys and fetch items
//
// Every extension *command* handler calls requireCapability before doing any
// work. A search key is not a command and a fetch item is not a command, so
// both classes escaped that gate entirely: `SEARCH FUZZY …` and
// `FETCH 1 (MODSEQ)` were parsed and handed to a backend that had witnessed
// neither SEARCH=FUZZY nor CONDSTORE.
//
// That is tolerable while this library owns both halves. It stops being
// tolerable the moment `imapserver/v0.1.0` exists, because then there is a
// population of third-party backends compiled against a fixed set of criteria
// and items. When package imap learns a new one — RFC 5257's ANNOTATION is the
// named candidate in docs/API-STABILITY.md §10 — every one of those backends
// starts receiving it, having witnessed nothing, and the realistic outcome is
// not a crash: it is a permissive default branch and a silently empty result.
//
// So the framework classifies instead. The tables below are data, which is what
// makes the next RFC a new row rather than a new code path, and
// TestEverySearchKeyAndFetchItemIsClassified reads the type declarations in
// package imap and fails when something is missing from them — because the
// failure mode of forgetting is to widen the gate, not to close it.
//
// Anything unclassified is refused rather than passed through. A criterion the
// framework cannot name is one it cannot gate, and handing it to a backend is
// the exact thing this exists to prevent.

// keyGate decides whether a session may use one search key or fetch item.
//
// It is a predicate over session state rather than a capability token, and that
// is the whole design. A token can only answer "did the backend witness this
// name", and membership in the FETCH or SEARCH grammar is not always a
// token question: a protocol revision can absorb an extension into the
// baseline, after which the key is legal for a session that enabled the
// revision whether or not the old token was ever advertised.
//
// BINARY[] is the case that proves it, and it proved it the hard way — the
// first version of this file gated on the "BINARY" token and answered
// NO [CANNOT] to a client it had just told to ENABLE IMAP4REV2. RFC 9051
// incorporates the BINARY *fetch* half; the token additionally claims the
// APPEND half, which rev2 did not incorporate, so the two are genuinely
// different questions. SERVER-DESIGN.md §1 calls this "the case where the
// distinction bites".
//
// The signature is deliberately identical to [featureDescriptor.Active]. The
// framework already had this shape one file over and already had BINARY right
// there; sharing the signature is what stops the two models drifting apart
// again, because a gate can simply *be* a feature's own predicate.
type keyGate func(state *sessionState, advertised map[string]bool) bool

// baselineKey is the IMAP4rev1 grammar: available to every session.
func baselineKey(*sessionState, map[string]bool) bool { return true }

// requiresToken gates on a capability the backend witnesses by name.
func requiresToken(name string) keyGate {
	return func(_ *sessionState, advertised map[string]bool) bool { return advertised[name] }
}

// requiresFeature gates on a framework feature, which may be activated by a
// token or by a protocol revision. See featureDescriptors in capability.go.
func requiresFeature(id featureID) keyGate {
	for _, descriptor := range featureDescriptors {
		if descriptor.ID == id {
			return func(state *sessionState, advertised map[string]bool) bool {
				return descriptor.Active(state, advertised) &&
					(descriptor.Requires == nil || descriptor.Requires(state))
			}
		}
	}
	// An unknown feature refuses rather than admits, matching the treatment of
	// an unclassified key. TestEveryKeyGateResolves is what stops it being
	// silent.
	return func(*sessionState, map[string]bool) bool { return false }
}

// baselineSearchKeywords are the argument-less keys of the IMAP4rev1 baseline.
// SearchKeyword is an open string type, so an unrecognised value is an
// extension key this framework has not been taught, not a baseline one.
var baselineSearchKeywords = map[imap.SearchKeyword]bool{
	imap.SearchAll: true, imap.SearchAnswered: true, imap.SearchDeleted: true,
	imap.SearchDraft: true, imap.SearchFlagged: true, imap.SearchNew: true,
	imap.SearchOld: true, imap.SearchRecent: true, imap.SearchSeen: true,
	imap.SearchUnanswered: true, imap.SearchUndeleted: true, imap.SearchUndraft: true,
	imap.SearchUnflagged: true, imap.SearchUnseen: true,
}

// searchKeywordCapabilities are argument-less keys an extension adds.
var searchKeywordCapabilities = map[imap.SearchKeyword]string{
	// RFC 8514 section 3: SAVEDATESUPPORTED asks whether the mailbox records
	// save dates at all.
	imap.SearchSaveDateSupported: "SAVEDATE",
}

// baselineSearchStringKeys are the string-argument keys of the baseline.
var baselineSearchStringKeys = map[imap.SearchStringKey]bool{
	imap.SearchKeyBcc: true, imap.SearchKeyBody: true, imap.SearchKeyCc: true,
	imap.SearchKeyFrom: true, imap.SearchKeySubject: true, imap.SearchKeyText: true,
	imap.SearchKeyTo: true,
}

// criterionCapability reports which capability a single criterion belongs to.
//
// An empty capability with classified true is a baseline key, available to
// every backend. classified false means the framework does not recognise the
// criterion, which is a refusal rather than a pass.
//
// Containers are classified with no capability of their own: the walk in
// requireCriteriaCapabilities descends into them, so gating a container would
// double-report its children.
func criterionCapability(criterion imap.SearchCriteria) (keyGate, bool) {
	switch criterion := criterion.(type) {
	case imap.SearchAnd, imap.SearchOr, imap.SearchNot:
		return keyGate(baselineKey), true
	case imap.SearchKeyword:
		if baselineSearchKeywords[criterion] {
			return keyGate(baselineKey), true
		}
		if capability, ok := searchKeywordCapabilities[criterion]; ok {
			return requiresToken(capability), true
		}
		return nil, false
	case imap.SearchString:
		if baselineSearchStringKeys[criterion.Key] {
			return keyGate(baselineKey), true
		}
		return nil, false
	case imap.SearchDate, imap.SearchSize, imap.SearchUID, imap.SearchSeqNum,
		imap.SearchFlagKeyword, imap.SearchHeaderField:
		return keyGate(baselineKey), true
	case imap.SearchModSeq:
		return requiresToken("CONDSTORE"), true
	case imap.SearchWithin:
		return requiresToken("WITHIN"), true
	case imap.SearchObjectID:
		return requiresToken("OBJECTID"), true
	case imap.SearchFuzzy:
		return requiresToken("SEARCH=FUZZY"), true
	case imap.SearchSavedResult:
		return requiresToken("SEARCHRES"), true
	case imap.SearchFilter:
		return requiresToken("FILTERS"), true
	default:
		return nil, false
	}
}

// fetchItemKeywordCapabilities are bare fetch-item names an extension adds.
var fetchItemKeywordCapabilities = map[imap.FetchItemKeyword]string{
	imap.FetchItemModSeq:   "CONDSTORE",
	imap.FetchItemEmailID:  "OBJECTID",
	imap.FetchItemThreadID: "OBJECTID",
	imap.FetchItemSaveDate: "SAVEDATE",
}

// baselineFetchItemKeywords are the bare item names of the baseline.
var baselineFetchItemKeywords = map[imap.FetchItemKeyword]bool{
	imap.FetchItemUID: true, imap.FetchItemFlags: true, imap.FetchItemInternalDate: true,
	imap.FetchItemRFC822Size: true, imap.FetchItemEnvelope: true, imap.FetchItemRFC822: true,
	imap.FetchItemRFC822Header: true, imap.FetchItemRFC822Text: true,
}

// fetchItemCapability is criterionCapability for the FETCH item list.
func fetchItemCapability(item imap.FetchItem) (keyGate, bool) {
	switch item := item.(type) {
	case imap.FetchItemKeyword:
		if baselineFetchItemKeywords[item] {
			return keyGate(baselineKey), true
		}
		if capability, ok := fetchItemKeywordCapabilities[item]; ok {
			return requiresToken(capability), true
		}
		return nil, false
	case *imap.FetchItemBodySection, *imap.FetchItemBodyStructure:
		return keyGate(baselineKey), true
	case *imap.FetchItemBinarySection, *imap.FetchItemBinarySectionSize:
		// Not the BINARY token. SERVER-DESIGN.md §1 calls this "the case where
		// the distinction bites": RFC 3516's FETCH half is incorporated into
		// IMAP4rev2, so BINARY[] is legal for a rev2 client whether or not the
		// BINARY capability — which additionally claims the APPEND half — is
		// advertised. featureBinaryFetch already encodes exactly that, so the
		// gate asks it rather than re-deriving the rule and getting it wrong.
		//
		// Gating on the token instead refused BINARY[] under rev2, which is how
		// this was found.
		return requiresFeature(featureBinaryFetch), true
	case *imap.FetchItemPreview:
		return requiresToken("PREVIEW"), true
	default:
		return nil, false
	}
}

// requireCriteriaCapabilities refuses a search whose criteria tree names a key
// this session has not been offered, at any nesting depth.
func requireCriteriaCapabilities(c *conn, criteria imap.SearchCriteria) error {
	if criteria == nil {
		return nil
	}
	gate, classified := criterionCapability(criteria)
	if !classified {
		return &imap.Error{
			Type: imap.ErrorTypeNo,
			Code: imap.CodeCannot,
			Text: fmt.Sprintf("unsupported search key %T", criteria),
		}
	}
	if !gate(&c.state, advertisedCapabilities(c)) {
		return &imap.Error{
			Type: imap.ErrorTypeNo,
			Code: imap.CodeCannot,
			Text: fmt.Sprintf("search key %T is not available in this session", criteria),
		}
	}
	if children, rebuild := searchCriteriaChildren(criteria); rebuild != nil {
		for _, child := range children {
			if err := requireCriteriaCapabilities(c, child); err != nil {
				return err
			}
		}
	}
	return nil
}

// requireFetchItemCapabilities refuses a FETCH naming an item this session has
// not been offered.
func requireFetchItemCapabilities(c *conn, items []imap.FetchItem) error {
	if len(items) == 0 {
		return nil
	}
	advertised := advertisedCapabilities(c)
	for _, item := range items {
		gate, classified := fetchItemCapability(item)
		if !classified {
			return &imap.Error{
				Type: imap.ErrorTypeNo,
				Code: imap.CodeCannot,
				Text: fmt.Sprintf("unsupported fetch item %T", item),
			}
		}
		if !gate(&c.state, advertised) {
			return &imap.Error{
				Type: imap.ErrorTypeNo,
				Code: imap.CodeCannot,
				Text: fmt.Sprintf("fetch item %T is not available in this session", item),
			}
		}
	}
	return nil
}
