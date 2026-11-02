package security

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// SessionCookieName is unchanged so the retained React frontend needs no change.
	SessionCookieName = "novel_session"
	// SessionLimit bounds session memory, including expired records awaiting pruning.
	SessionLimit = 256
	// SessionIdleTimeout is sliding, with the legacy strict greater-than boundary.
	SessionIdleTimeout = 24 * time.Hour
	// SessionPressureIdleTimeout applies ONLY when creating at capacity, as before.
	SessionPressureIdleTimeout = time.Hour
)

// Session is an immutable identity and CSRF capability owned by one Sessions
// store. Access it only through its getters. Pointer identity is stable on reuse
// and suitable for download/stream ownership; a changed principal gets an entirely
// new session. Mutable runtime clients and their 32-tab cap belong to the parent.
type Session struct {
	id        string
	csrf      string
	principal string
	owner     *Sessions
	// touched is protected by owner.mu; callers never mutate it.
	touched time.Time
}

// ID returns a cryptographically random 32-byte, lowercase hex session token.
func (s *Session) ID() string { return s.id }

// CSRFToken returns the separate 32-byte lowercase hex bootstrap/CSRF token.
// It is intended for the authenticated bootstrap JSON, never a readable cookie.
func (s *Session) CSRFToken() string { return s.csrf }

// Principal returns the exact identity supplied by the authenticating proxy.
func (s *Session) Principal() string { return s.principal }

// Sessions is a concurrency-safe, bounded, in-memory session store. It must not
// be copied after use. Its zero value is ready to use. There are no background
// goroutines, persistence, credential reads or unbounded expired-session queues.
type Sessions struct {
	mu       sync.Mutex
	sessions map[string]*Session
	// Internal clock/entropy injection permits deterministic synthetic tests.
	now     func() time.Time
	entropy io.Reader
}

// NewSessions creates a store using time.Now and crypto/rand.Reader.
func NewSessions() *Sessions { return &Sessions{} }

// Get obtains or creates a session for the exact trusted principal, touching its
// sliding idle deadline. Authenticate must succeed and match principal; callers
// cannot use a cookie alone to override the proxy identity. The first matching
// novel_session cookie wins, as in Node. New sessions set exactly HttpOnly,
// SameSite=Strict, Path=/, plus Secure ONLY for exact X-Forwarded-Proto: https.
// No Domain, Max-Age or Expires is added. Existing sessions do not reissue cookies.
// At capacity, only records idle strictly over one hour are evicted; otherwise
// the legacy 503 SESSION_LIMIT is returned. Expiry is strictly over 24 hours.
func (s *Sessions) Get(r *http.Request, w http.ResponseWriter, principal string) (*Session, error) {
	identity, err := Authenticate(r)
	if err != nil {
		return nil, err
	}
	if !ConstantEqual(identity, principal) {
		return nil, NewHTTPError(http.StatusUnauthorized, "AUTHENTICATION_REQUIRED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]*Session)
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	var token string
	cookies := strings.Join(r.Header.Values("Cookie"), ";")
	for _, part := range strings.Split(cookies, ";") {
		part = strings.TrimFunc(part, jsWhitespace)
		if strings.HasPrefix(part, SessionCookieName+"=") {
			token = strings.TrimPrefix(part, SessionCookieName+"=")
			break
		}
	}
	session := s.sessions[token]
	if session != nil && (!ConstantEqual(session.principal, principal) || now.Sub(session.touched) > SessionIdleTimeout) {
		session = nil
	}
	if session == nil {
		if len(s.sessions) >= SessionLimit {
			for key, old := range s.sessions {
				if now.Sub(old.touched) > SessionPressureIdleTimeout {
					delete(s.sessions, key)
				}
			}
			if len(s.sessions) >= SessionLimit {
				return nil, NewHTTPError(http.StatusServiceUnavailable, "SESSION_LIMIT")
			}
		}
		entropy := s.entropy
		if entropy == nil {
			entropy = rand.Reader
		}
		var id string
		// Reject an entropy-source collision rather than overwriting another
		// identity. Real crypto/rand collisions are astronomically unlikely.
		for attempt := 0; attempt < 8; attempt++ {
			id, err = randomToken(entropy)
			if err != nil {
				return nil, err
			}
			if _, exists := s.sessions[id]; !exists {
				break
			}
			id = ""
		}
		if id == "" {
			return nil, fmt.Errorf("session entropy collision")
		}
		csrf, err := randomToken(entropy)
		if err != nil {
			return nil, err
		}
		session = &Session{id: id, csrf: csrf, principal: principal, owner: s, touched: now}
		s.sessions[id] = session
		cookie := SessionCookieName + "=" + id + "; HttpOnly; SameSite=Strict; Path=/"
		if proto, ok := singleHeader(r, "X-Forwarded-Proto"); ok && proto == "https" {
			cookie += "; Secure"
		}
		w.Header().Set("Set-Cookie", cookie)
	}
	session.touched = now
	return session, nil
}

func randomToken(entropy io.Reader) (string, error) {
	var bytes [32]byte
	if _, err := io.ReadFull(entropy, bytes[:]); err != nil {
		return "", fmt.Errorf("session entropy unavailable: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

// Verify checks Origin, Sec-Fetch-Site and X-CSRF-Token, preserving error order.
// Always call Get with the current request first: this enforces idle expiry and
// trusted-principal binding before a parent performs any operation. A Session
// obtained from a different Sessions instance is never accepted.
func (s *Sessions) Verify(r *http.Request, session *Session, allowed Origins) error {
	if err := CheckOrigin(r, allowed); err != nil {
		return err
	}
	token, ok := singleHeader(r, "X-CSRF-Token")
	principal, identityErr := Authenticate(r)
	if !ok || identityErr != nil || session == nil || session.owner != s || !ConstantEqual(principal, session.principal) || !ConstantEqual(token, session.csrf) {
		return NewHTTPError(http.StatusForbidden, "CSRF_DENIED")
	}
	return nil
}
