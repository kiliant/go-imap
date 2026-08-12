package imapserver

import (
	"errors"
	"strings"
)

// Framework errors returned to backend and server callers.
var (
	// ErrWriterClosed means a backend retained a framework writer past the
	// method call that owned it, or used its zero value.
	ErrWriterClosed = errors.New("imapserver: writer is closed")
	// ErrUpdaterClosed means the selection associated with an Updater ended.
	ErrUpdaterClosed = errors.New("imapserver: updater is closed")
	// ErrUpdateOverflow means the bounded update queue overflowed. The
	// connection is terminated rather than silently dropping a removal.
	ErrUpdateOverflow = errors.New("imapserver: update queue overflow")
	// ErrRevisionMismatch means an update batch did not continue the selected
	// snapshot's revision chain. The connection cannot safely continue.
	ErrRevisionMismatch = errors.New("imapserver: mailbox revision mismatch")
)

type connectionState uint8

const (
	stateNotAuthenticated connectionState = iota
	stateAuthenticated
	stateSelected
)

type stateMask uint8

const (
	stateMaskNotAuthenticated stateMask = 1 << iota
	stateMaskAuthenticated
	stateMaskSelected
	stateMaskAny = stateMaskNotAuthenticated | stateMaskAuthenticated | stateMaskSelected
)

func (s connectionState) mask() stateMask {
	switch s {
	case stateNotAuthenticated:
		return stateMaskNotAuthenticated
	case stateAuthenticated:
		return stateMaskAuthenticated
	case stateSelected:
		return stateMaskSelected
	default:
		return 0
	}
}

type protocolRevision uint8

const (
	revisionIMAP4rev1 protocolRevision = iota + 1
	revisionIMAP4rev2
)

type sessionState struct {
	state      connectionState
	revision   protocolRevision
	enabled    map[string]bool
	tls        bool
	compressed bool
	session    Session
	selected   *selectedState
}

func newSessionState(tlsActive bool) sessionState {
	return sessionState{
		state:    stateNotAuthenticated,
		revision: revisionIMAP4rev1,
		enabled:  make(map[string]bool),
		tls:      tlsActive,
	}
}

func (s *sessionState) allows(mask stateMask) bool {
	return s != nil && mask&s.state.mask() != 0
}

func (s *sessionState) authenticate(session Session) bool {
	if s == nil || s.state != stateNotAuthenticated || session == nil {
		return false
	}
	s.session = session
	s.state = stateAuthenticated
	return true
}

func (s *sessionState) selectMailbox(selected *selectedState) bool {
	if s == nil || s.state == stateNotAuthenticated || selected == nil {
		return false
	}
	if s.revision == revisionIMAP4rev2 {
		if _, ok := selected.mailbox.(MoveMailbox); !ok {
			return false
		}
	}
	s.selected = selected
	s.state = stateSelected
	return true
}

func (s *sessionState) unselect() *selectedState {
	if s == nil || s.state != stateSelected {
		return nil
	}
	selected := s.selected
	s.selected = nil
	s.state = stateAuthenticated
	return selected
}

func (s *sessionState) enable(capability string) bool {
	capability = strings.ToUpper(capability)
	switch capability {
	case "IMAP4REV2":
		s.revision = revisionIMAP4rev2
	case "UTF8=ACCEPT":
	default:
		return false
	}
	s.enabled[capability] = true
	return true
}

func (s *sessionState) enabledCapability(capability string) bool {
	return s != nil && s.enabled[strings.ToUpper(capability)]
}
