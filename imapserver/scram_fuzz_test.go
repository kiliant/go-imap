package imapserver

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The SCRAM messages are parsed before the client has proved anything, so every
// byte here arrives from an unauthenticated remote peer. T23's sign-off named
// these the highest-priority fuzz targets in imapserver for that reason.
//
// Each target asserts the security property the parser exists to enforce, not
// merely that it returned. A parser that stops panicking but starts accepting a
// downgraded exchange has got worse, and a target that only checked for panics
// would score that as a pass.

// FuzzParseSCRAMClientFirst drives the GS2 header and channel-binding decisions.
func FuzzParseSCRAMClientFirst(f *testing.F) {
	for _, seed := range []string{
		"n,,n=alice,r=cnonce",
		"y,,n=alice,r=cnonce",
		"p=tls-exporter,,n=alice,r=cnonce",
		"n,a=admin,n=alice,r=cnonce",
		"n,a=od=2Cd,n=al=3Dice,r=cnonce",
		"n,,r=cnonce",
		"n,,n=alice",
		"n,,",
		"n",
		"",
		",,",
		"p=tls-unique,,n=alice,r=cnonce",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, message string) {
		for _, bound := range []bool{false, true} {
			for _, plusAdvertised := range []bool{false, true} {
				bare, username, authzID, nonce, err := parseSCRAMClientFirst(message, bound, plusAdvertised)
				if err != nil {
					// A rejected message must not leak a partially parsed
					// identity: the caller distinguishes the two only by err.
					if bare != "" || username != "" || authzID != "" || nonce != "" {
						t.Fatalf("parseSCRAMClientFirst(%q, %v, %v) returned %q/%q/%q/%q alongside error %v",
							message, bound, plusAdvertised, bare, username, authzID, nonce, err)
					}
					continue
				}

				// Acceptance implies both identity fields are present; the
				// caller indexes credentials by username and echoes the nonce.
				if username == "" || nonce == "" {
					t.Fatalf("parseSCRAMClientFirst(%q, %v, %v) accepted with username=%q nonce=%q",
						message, bound, plusAdvertised, username, nonce)
				}

				// bare is concatenated into the auth message that the proof is
				// computed over, so it must be a genuine suffix of what the
				// client sent rather than anything synthesised here.
				if !strings.HasSuffix(message, bare) {
					t.Fatalf("parseSCRAMClientFirst(%q, %v, %v) returned bare %q that is not a suffix of the message",
						message, bound, plusAdvertised, bare)
				}

				header, _, _ := strings.Cut(message, ",")
				switch {
				case bound:
					// RFC 5802 section 6: a -PLUS mechanism must name the
					// binding type this server actually applies.
					if header != "p=tls-exporter" {
						t.Fatalf("parseSCRAMClientFirst(%q, bound, %v) accepted GS2 header %q",
							message, plusAdvertised, header)
					}
				case header == "y" && plusAdvertised:
					// The downgrade detection. "y" claims the server offers no
					// -PLUS mechanism; if one is advertised, the claim can only
					// come from an attacker stripping it in transit.
					t.Fatalf("parseSCRAMClientFirst(%q, unbound, plusAdvertised) accepted a stripped -PLUS downgrade", message)
				case header != "n" && header != "y":
					t.Fatalf("parseSCRAMClientFirst(%q, unbound, %v) accepted unknown GS2 header %q",
						message, plusAdvertised, header)
				}
			}
		}
	})
}

// FuzzParseSCRAMClientFinal drives proof decoding, nonce comparison and the
// channel-binding check that makes a -PLUS mechanism stronger than its
// unbound form.
func FuzzParseSCRAMClientFinal(f *testing.F) {
	const nonce = "cnoncesnonce"
	for _, seed := range []string{
		"c=biws,r=" + nonce + ",p=" + base64.StdEncoding.EncodeToString([]byte("proof")),
		"c=biws,r=" + nonce + ",p=",
		"c=biws,r=" + nonce + ",p=!!!not base64!!!",
		"c=biws,r=wrongnonce,p=cHJvb2Y=",
		"c=,r=" + nonce + ",p=cHJvb2Y=",
		"r=" + nonce + ",p=cHJvb2Y=",
		"c=biws,r=" + nonce,
		",p=",
		"p=",
		"",
	} {
		f.Add(seed, nonce, "n,,", []byte(nil))
	}
	f.Fuzz(func(t *testing.T, message, expectedNonce, header string, binding []byte) {
		withoutProof, proof, err := parseSCRAMClientFinal(message, expectedNonce, header, binding)
		if err != nil {
			if withoutProof != "" || proof != nil {
				t.Fatalf("parseSCRAMClientFinal(%q, %q, %q, %x) returned %q/%x alongside error %v",
					message, expectedNonce, header, binding, withoutProof, proof, err)
			}
			return
		}

		// withoutProof is the second half of the auth message the proof is
		// verified against, so like bare above it must come from the wire.
		if !strings.HasPrefix(message, withoutProof) {
			t.Fatalf("parseSCRAMClientFinal(%q, ...) returned withoutProof %q that is not a prefix of the message",
				message, withoutProof)
		}

		var gotNonce, gotChannel string
		for _, field := range strings.Split(withoutProof, ",") {
			if value, ok := strings.CutPrefix(field, "r="); ok {
				gotNonce = value
			}
			if value, ok := strings.CutPrefix(field, "c="); ok {
				gotChannel = value
			}
		}
		// Acceptance means this exchange is bound to the server-first message
		// this connection sent. Without the nonce equality a client-final
		// captured from another exchange would replay.
		if gotNonce != expectedNonce {
			t.Fatalf("parseSCRAMClientFinal accepted %q against expected nonce %q (parsed %q)",
				message, expectedNonce, gotNonce)
		}
		// And bound to this connection's channel binding: the c= field must
		// repeat the GS2 header plus the binding data, or a -PLUS mechanism
		// buys nothing over its unbound form.
		wantChannel := base64.StdEncoding.EncodeToString(append([]byte(header), binding...))
		if gotChannel != wantChannel {
			t.Fatalf("parseSCRAMClientFinal accepted %q with channel %q, want %q",
				message, gotChannel, wantChannel)
		}
	})
}
