package imapserver

// BINARY (RFC 3516) and the UTF8=* family (RFC 5738, RFC 9755).
//
// # Why BINARY is two features, not one
//
// SERVER-DESIGN.md §1 splits this deliberately and it is worth restating where
// the code is. BINARY *FETCH* — `BINARY[]` and `BINARY.SIZE[]` — is available
// when IMAP4rev2 is enabled *or* when BINARY is advertised to a rev1 client,
// because rev2 incorporates the fetch side. Binary *APPEND* — a literal8
// payload — requires the BINARY capability specifically, since rev2 did not
// incorporate it.
//
// Those are the featureBinaryFetch and featureBinaryAppend descriptors that
// already exist in capability.go. This file supplies the capability token they
// key on, which was the missing half: the features were defined but no
// descriptor ever advertised BINARY, so featureBinaryAppend could never be
// active and featureBinaryFetch only ever fired under rev2.

func init() {
	registerCapabilities(
		// BINARY needs the backend to decode content-transfer-encoding, which
		// the framework cannot do for it: it has no access to the message.
		capabilityDescriptor{
			Name:            "BINARY",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("BINARY"),
		},
	)
}
