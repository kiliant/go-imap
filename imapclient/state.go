package imapclient

// State is the session state defined by RFC 3501 section 3.  It is a
// string-backed type so diagnostic tools can retain a state introduced by a
// future protocol revision.
type State string

const (
	// StateNotAuthenticated is the state after an OK greeting and before
	// authentication.
	StateNotAuthenticated State = "not-authenticated"
	// StateAuthenticated is the state after authentication and before SELECT or
	// EXAMINE.
	StateAuthenticated State = "authenticated"
	// StateSelected is the state while a mailbox is selected.
	StateSelected State = "selected"
	// StateLogout is the terminal state after a BYE response or Close.
	StateLogout State = "logout"
)

type stateMask uint8

const (
	stateNotAuthenticated stateMask = 1 << iota
	stateAuthenticated
	stateSelected
)

func (s State) mask() stateMask {
	switch s {
	case StateNotAuthenticated:
		return stateNotAuthenticated
	case StateAuthenticated:
		return stateAuthenticated
	case StateSelected:
		return stateSelected
	default:
		return 0
	}
}
