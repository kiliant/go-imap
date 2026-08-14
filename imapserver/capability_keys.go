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
func criterionCapability(criterion imap.SearchCriteria) (string, bool) {
	switch criterion := criterion.(type) {
	case imap.SearchAnd, imap.SearchOr, imap.SearchNot:
		return "", true
	case imap.SearchKeyword:
		if baselineSearchKeywords[criterion] {
			return "", true
		}
		if capability, ok := searchKeywordCapabilities[criterion]; ok {
			return capability, true
		}
		return "", false
	case imap.SearchString:
		if baselineSearchStringKeys[criterion.Key] {
			return "", true
		}
		return "", false
	case imap.SearchDate, imap.SearchSize, imap.SearchUID, imap.SearchSeqNum,
		imap.SearchFlagKeyword, imap.SearchHeaderField:
		return "", true
	case imap.SearchModSeq:
		return "CONDSTORE", true
	case imap.SearchWithin:
		return "WITHIN", true
	case imap.SearchObjectID:
		return "OBJECTID", true
	case imap.SearchFuzzy:
		return "SEARCH=FUZZY", true
	case imap.SearchSavedResult:
		return "SEARCHRES", true
	case imap.SearchFilter:
		return "FILTERS", true
	default:
		return "", false
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
func fetchItemCapability(item imap.FetchItem) (string, bool) {
	switch item := item.(type) {
	case imap.FetchItemKeyword:
		if baselineFetchItemKeywords[item] {
			return "", true
		}
		if capability, ok := fetchItemKeywordCapabilities[item]; ok {
			return capability, true
		}
		return "", false
	case *imap.FetchItemBodySection, *imap.FetchItemBodyStructure:
		return "", true
	case *imap.FetchItemBinarySection, *imap.FetchItemBinarySectionSize:
		// RFC 3516's FETCH half is incorporated into IMAP4rev2, but the BINARY
		// token claims the APPEND half too. The witness is what a backend
		// actually agreed to, so gate on it either way.
		return "BINARY", true
	case *imap.FetchItemPreview:
		return "PREVIEW", true
	default:
		return "", false
	}
}

// requireCriteriaCapabilities refuses a search whose criteria tree names a key
// this session has not been offered, at any nesting depth.
func requireCriteriaCapabilities(c *conn, criteria imap.SearchCriteria) error {
	if criteria == nil {
		return nil
	}
	capability, classified := criterionCapability(criteria)
	if !classified {
		return &imap.Error{
			Type: imap.ErrorTypeNo,
			Code: imap.CodeCannot,
			Text: fmt.Sprintf("unsupported search key %T", criteria),
		}
	}
	if capability != "" && !advertisedCapabilities(c)[capability] {
		return &imap.Error{
			Type: imap.ErrorTypeNo,
			Code: imap.CodeCannot,
			Text: fmt.Sprintf("search key requires the %s capability", capability),
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
	for _, item := range items {
		capability, classified := fetchItemCapability(item)
		if !classified {
			return &imap.Error{
				Type: imap.ErrorTypeNo,
				Code: imap.CodeCannot,
				Text: fmt.Sprintf("unsupported fetch item %T", item),
			}
		}
		if capability != "" && !advertisedCapabilities(c)[capability] {
			return &imap.Error{
				Type: imap.ErrorTypeNo,
				Code: imap.CodeCannot,
				Text: fmt.Sprintf("fetch item requires the %s capability", capability),
			}
		}
	}
	return nil
}
