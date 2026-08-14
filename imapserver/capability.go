package imapserver

import (
	"slices"
	"strings"
)

type tlsRequirement uint8

const (
	tlsEither tlsRequirement = iota
	tlsOnly
	plaintextOnly
)

type frameworkComponent string

const (
	frameworkCore       frameworkComponent = "core"
	frameworkStartTLS   frameworkComponent = "starttls"
	frameworkAuth       frameworkComponent = "auth"
	frameworkEnable     frameworkComponent = "enable"
	frameworkUTF8       frameworkComponent = "utf8"
	frameworkIdle       frameworkComponent = "idle"
	frameworkCompress   frameworkComponent = "compress"
	frameworkMove       frameworkComponent = "move"
	frameworkRev2       frameworkComponent = "rev2"
	frameworkListExtend frameworkComponent = "list-extended"
)

type capabilityDescriptor struct {
	Name              string
	RequiresBackend   func(*sessionState, Backend) bool
	RequiresFramework []frameworkComponent
	Depends           []string
	States            stateMask
	RequiresTLS       tlsRequirement
	Enable            func(*sessionState) bool
	Available         func(*sessionState, *Server) bool
}

// capabilityDescriptors is the only source of CAPABILITY output. Commands and
// greetings call deriveCapabilities; they never maintain parallel token lists.
var capabilityDescriptors = []capabilityDescriptor{
	{Name: "IMAP4REV1", RequiresFramework: []frameworkComponent{frameworkCore}, States: stateMaskAny},
	{Name: "STARTTLS", RequiresFramework: []frameworkComponent{frameworkStartTLS}, States: stateMaskNotAuthenticated, RequiresTLS: plaintextOnly,
		Available: func(_ *sessionState, server *Server) bool { return server != nil && server.options.TLSConfig != nil }},
	{Name: "LOGINDISABLED", RequiresFramework: []frameworkComponent{frameworkCore}, States: stateMaskNotAuthenticated,
		Available: func(state *sessionState, server *Server) bool {
			return server != nil && (server.backend == nil || state != nil && !state.tls &&
				(server.options.RequireTLS || !server.options.AllowInsecureAuth))
		}},
	{Name: "ENABLE", RequiresFramework: []frameworkComponent{frameworkEnable}, States: stateMaskAny},
	{Name: "ID", RequiresFramework: []frameworkComponent{frameworkCore}, States: stateMaskAny},
	// UIDPLUS (RFC 4315) is three things: the APPENDUID and COPYUID response
	// codes, and the UID EXPUNGE command. Until T24 the server emitted the two
	// codes and implemented neither the command nor the advertisement, so no
	// conforming client could act on any of it.
	//
	// Witnessed rather than framework-only. UID EXPUNGE works for any backend —
	// the Expunge contract has always carried the UID-set filter — but the two
	// response codes exist only if the backend returns UIDs in AppendData and
	// CopyData, and RFC 4315 section 3 requires a UIDPLUS server to send them.
	// A backend that cannot must not have the claim made on its behalf.
	{Name: "UIDPLUS", RequiresFramework: []frameworkComponent{frameworkCore},
		States:          stateMaskAuthenticated | stateMaskSelected,
		RequiresBackend: backendSupportsCapability("UIDPLUS")},
	{Name: "LITERAL-", RequiresFramework: []frameworkComponent{frameworkCore}, States: stateMaskAny},
	{Name: "SASL-IR", RequiresFramework: []frameworkComponent{frameworkAuth}, States: stateMaskNotAuthenticated,
		RequiresBackend: hasAuthenticationBackend},
	{Name: "AUTH=PLAIN", RequiresFramework: []frameworkComponent{frameworkAuth}, States: stateMaskNotAuthenticated,
		RequiresBackend: hasAuthenticationBackend,
		Available: func(state *sessionState, server *Server) bool {
			return state != nil && (state.tls || (server != nil && !server.options.RequireTLS && server.options.AllowInsecureAuth))
		}},
	{Name: "AUTH=LOGIN", RequiresFramework: []frameworkComponent{frameworkAuth}, States: stateMaskNotAuthenticated,
		RequiresBackend: hasAuthenticationBackend,
		Available: func(state *sessionState, server *Server) bool {
			return state != nil && (state.tls || (server != nil && !server.options.RequireTLS && server.options.AllowInsecureAuth))
		}},
	{Name: "AUTH=XOAUTH2", RequiresFramework: []frameworkComponent{frameworkAuth}, States: stateMaskNotAuthenticated,
		RequiresBackend: hasAuthenticationBackend, RequiresTLS: tlsOnly},
	{Name: "AUTH=OAUTHBEARER", RequiresFramework: []frameworkComponent{frameworkAuth}, States: stateMaskNotAuthenticated,
		RequiresBackend: hasAuthenticationBackend, RequiresTLS: tlsOnly},
	{Name: "UTF8=ACCEPT", RequiresFramework: []frameworkComponent{frameworkUTF8}, States: stateMaskAuthenticated | stateMaskSelected,
		Enable: func(state *sessionState) bool { return state.enable("UTF8=ACCEPT") }},
	{Name: "IDLE", RequiresFramework: []frameworkComponent{frameworkIdle}, States: stateMaskAuthenticated | stateMaskSelected},
	{Name: "COMPRESS=DEFLATE", RequiresFramework: []frameworkComponent{frameworkCompress}, States: stateMaskAuthenticated | stateMaskSelected,
		Available: func(state *sessionState, _ *Server) bool { return state != nil && !state.compressed }},
	{Name: "UNSELECT", RequiresFramework: []frameworkComponent{frameworkCore}, States: stateMaskAuthenticated | stateMaskSelected},
	{Name: "LIST-EXTENDED", RequiresFramework: []frameworkComponent{frameworkListExtend}, States: stateMaskAuthenticated | stateMaskSelected},
	{Name: "MOVE", RequiresFramework: []frameworkComponent{frameworkMove}, States: stateMaskAuthenticated | stateMaskSelected,
		RequiresBackend: supportsAtomicMove},
	{Name: "IMAP4REV2", RequiresFramework: []frameworkComponent{frameworkRev2, frameworkMove}, States: stateMaskAny,
		RequiresBackend: supportsAtomicMove,
		Enable:          func(state *sessionState) bool { return state.enable("IMAP4REV2") }},
}

func hasAuthenticationBackend(_ *sessionState, backend Backend) bool { return backend != nil }

func supportsAtomicMove(state *sessionState, backend Backend) bool {
	var support MoveSupport
	if state != nil && state.session != nil {
		support, _ = state.session.(MoveSupport)
	}
	if support == nil {
		support, _ = backend.(MoveSupport)
	}
	if support == nil || !support.SupportsMove() {
		return false
	}
	if state != nil && state.selected != nil {
		_, ok := state.selected.mailbox.(MoveMailbox)
		return ok
	}
	return true
}

func compiledFrameworkSupport() map[frameworkComponent]bool {
	// This registry is deliberately tied to compiled command handlers. A
	// capability is never advertised merely because a backend happens to expose
	// a similarly named operation.
	return map[frameworkComponent]bool{
		frameworkCore:     true,
		frameworkStartTLS: true,
		frameworkAuth:     true,
		frameworkEnable:   true,
		frameworkUTF8:     true,
		frameworkIdle:     true,
		frameworkCompress: true,
		frameworkMove:     true,
		frameworkRev2:     true,
		// LIST-EXTENDED's selection, return and multi-pattern handling is
		// compiled in as of T23's group A. See ext_a_list.go.
		frameworkListExtend: true,
	}
}

func deriveCapabilities(state *sessionState, server *Server) []string {
	if state == nil || server == nil {
		return nil
	}
	support := server.framework
	available := make(map[string]bool, len(capabilityDescriptors))
	for _, descriptor := range capabilityDescriptors {
		if !state.allows(descriptor.States) || !tlsMatches(state.tls, descriptor.RequiresTLS) {
			continue
		}
		ok := true
		for _, component := range descriptor.RequiresFramework {
			if !support[component] {
				ok = false
				break
			}
		}
		if !ok || descriptor.RequiresBackend != nil && !descriptor.RequiresBackend(state, server.backend) ||
			descriptor.Available != nil && !descriptor.Available(state, server) {
			continue
		}
		available[descriptor.Name] = true
	}

	// Dependencies are checked after the first pass so descriptor order cannot
	// accidentally make one token available.
	var capabilities []string
	for _, descriptor := range capabilityDescriptors {
		if !available[descriptor.Name] {
			continue
		}
		ok := true
		for _, dependency := range descriptor.Depends {
			if !available[dependency] {
				ok = false
				break
			}
		}
		if ok {
			capabilities = append(capabilities, descriptor.Name)
		}
	}
	// Capabilities whose advertised token embeds a backend-supplied value are
	// rewritten here. See ext_d_listret.go.
	capabilities = capabilityValueOverrides(state, capabilities)
	slices.Sort(capabilities)
	if at := slices.Index(capabilities, "IMAP4REV1"); at > 0 {
		capabilities[0], capabilities[at] = capabilities[at], capabilities[0]
	}
	return capabilities
}

func tlsMatches(active bool, requirement tlsRequirement) bool {
	switch requirement {
	case tlsOnly:
		return active
	case plaintextOnly:
		return !active
	default:
		return true
	}
}

func enableCapabilities(state *sessionState, server *Server, requested []string) []string {
	advertised := deriveCapabilities(state, server)
	var enabled []string
	seen := make(map[string]bool)
	for _, name := range requested {
		name = strings.ToUpper(name)
		if seen[name] || !slices.Contains(advertised, name) {
			continue
		}
		seen[name] = true
		for _, descriptor := range capabilityDescriptors {
			if descriptor.Name == name && descriptor.Enable != nil && descriptor.Enable(state) {
				enabled = append(enabled, name)
			}
		}
	}
	return enabled
}

type featureID string

const (
	featureBinaryFetch   featureID = "binary-fetch"
	featureBinaryAppend  featureID = "binary-append"
	featureListMulti     featureID = "list-multi-pattern"
	featureListSubscribe featureID = "list-subscribed"
)

type featureDescriptor struct {
	ID       featureID
	Active   func(*sessionState, map[string]bool) bool
	Requires func(*sessionState) bool
}

var featureDescriptors = []featureDescriptor{
	{ID: featureBinaryFetch, Active: func(state *sessionState, advertised map[string]bool) bool {
		return state.revision == revisionIMAP4rev2 || advertised["BINARY"]
	}},
	{ID: featureBinaryAppend, Active: func(_ *sessionState, advertised map[string]bool) bool { return advertised["BINARY"] }},
	{ID: featureListMulti, Active: func(state *sessionState, advertised map[string]bool) bool {
		return state.revision == revisionIMAP4rev2 || advertised["LIST-EXTENDED"]
	}},
	{ID: featureListSubscribe, Active: func(state *sessionState, advertised map[string]bool) bool {
		return state.revision == revisionIMAP4rev2 || advertised["LIST-EXTENDED"]
	}},
}

func activeFeatures(state *sessionState, server *Server) map[featureID]bool {
	capabilities := make(map[string]bool)
	for _, capability := range deriveCapabilities(state, server) {
		capabilities[capability] = true
	}
	features := make(map[featureID]bool)
	for _, descriptor := range featureDescriptors {
		if descriptor.Active(state, capabilities) && (descriptor.Requires == nil || descriptor.Requires(state)) {
			features[descriptor.ID] = true
		}
	}
	return features
}
