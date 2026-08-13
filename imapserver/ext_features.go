package imapserver

// Feature identifiers for the extension option fields declared on the shared
// option structs in backend.go. A field there binds to one of these through its
// imapfeature struct tag, and the framework populates the field only when the
// feature is active for the session.
//
// These live here, rather than beside the capability descriptors, because the
// fields they bind are frozen ahead of the individual extension groups: an
// options field cannot be added by the group that needs it without four groups
// editing one file. A group that needs a feature bound to nothing outside its
// own files registers it from its own file instead.
const (
	featureListExtended     featureID = "list-extended"
	featureListChildren     featureID = "list-children"
	featureListSpecialUse   featureID = "list-special-use"
	featureCreateSpecialUse featureID = "create-special-use"
	featureCondStoreSelect  featureID = "condstore-select"
	featureCondStoreFetch   featureID = "condstore-fetch"
	featureCondStoreStore   featureID = "condstore-store"
	featureQResync          featureID = "qresync"
)

func init() {
	registerFeatures(
		// LIST selection and return options beyond SUBSCRIBED. IMAP4rev2
		// incorporates the LIST-EXTENDED syntax, so the capability token is not
		// the only route to the behaviour.
		featureDescriptor{ID: featureListExtended, Active: func(state *sessionState, advertised map[string]bool) bool {
			return state.revision == revisionIMAP4rev2 || advertised["LIST-EXTENDED"]
		}},
		// rev2 folds RFC 3348's child attributes into the base LIST response.
		featureDescriptor{ID: featureListChildren, Active: func(state *sessionState, advertised map[string]bool) bool {
			return state.revision == revisionIMAP4rev2 || advertised["CHILDREN"]
		}},
		// rev2 defines the special-use attributes but not CREATE's USE
		// parameter, which stays gated on RFC 6154's own token.
		featureDescriptor{ID: featureListSpecialUse, Active: func(state *sessionState, advertised map[string]bool) bool {
			return state.revision == revisionIMAP4rev2 || advertised["SPECIAL-USE"]
		}},
		featureDescriptor{ID: featureCreateSpecialUse, Active: func(_ *sessionState, advertised map[string]bool) bool {
			return advertised["CREATE-SPECIAL-USE"]
		}},
		// CONDSTORE and QRESYNC are not incorporated into rev2 and are active
		// only on their own tokens. Each stays inactive until the group that
		// owns it registers the capability descriptor.
		featureDescriptor{ID: featureCondStoreSelect, Active: condStoreActive},
		featureDescriptor{ID: featureCondStoreFetch, Active: condStoreActive},
		featureDescriptor{ID: featureCondStoreStore, Active: condStoreActive},
		featureDescriptor{ID: featureQResync, Active: func(_ *sessionState, advertised map[string]bool) bool {
			return advertised["QRESYNC"]
		}},
	)
}

func condStoreActive(_ *sessionState, advertised map[string]bool) bool {
	return advertised["CONDSTORE"] || advertised["QRESYNC"]
}

// registerFeatures adds feature descriptors to the framework's table. It is
// called from init, so that an extension group contributes its own descriptors
// from its own file rather than editing a shared table.
func registerFeatures(descriptors ...featureDescriptor) {
	for _, descriptor := range descriptors {
		for _, existing := range featureDescriptors {
			if existing.ID == descriptor.ID {
				panic("imapserver: duplicate feature descriptor for " + string(descriptor.ID))
			}
		}
		featureDescriptors = append(featureDescriptors, descriptor)
	}
}

// registerCapabilities adds capability descriptors to the framework's table,
// on the same terms as registerFeatures.
func registerCapabilities(descriptors ...capabilityDescriptor) {
	for _, descriptor := range descriptors {
		for _, existing := range capabilityDescriptors {
			if existing.Name == descriptor.Name {
				panic("imapserver: duplicate capability descriptor for " + descriptor.Name)
			}
		}
		capabilityDescriptors = append(capabilityDescriptors, descriptor)
	}
}

// backendSupportsCapability reports whether the backend witnesses the named
// capability through [CapabilitySupport]. The session's witness wins when it
// has one, so support can vary by authenticated user.
func backendSupportsCapability(name string) func(*sessionState, Backend) bool {
	return func(state *sessionState, backend Backend) bool {
		if state != nil && state.session != nil {
			if support, ok := state.session.(CapabilitySupport); ok {
				return support.SupportsCapability(name)
			}
		}
		if support, ok := backend.(CapabilitySupport); ok {
			return support.SupportsCapability(name)
		}
		return false
	}
}
