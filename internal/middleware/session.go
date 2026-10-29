package middleware

import (
	"sync"
	"time"
)

// SessionStore manages Cookie-based sessions
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*sessionEntry
	ttl      int64 // milliseconds
}

type sessionEntry struct {
	apiKey    string
	expires   int64
	createdAt int64
}

const sessionCookieName = "substore_sid"

// NewSessionStore creates a new session store
func NewSessionStore(ttl int64) *SessionStore {
	return &SessionStore{
		sessions: make(map[string]*sessionEntry),
		ttl:      ttl,
	}
}

// CreateSession creates a new session and returns the session ID
func (ss *SessionStore) Create(apiKey string) string {
	sid := randomHex(32)
	ss.mu.Lock()
	ss.sessions[sid] = &sessionEntry{
		apiKey:    apiKey,
		expires:   time.Now().UnixMilli() + ss.ttl,
		createdAt: time.Now().UnixMilli(),
	}
	ss.mu.Unlock()
	return sid
}

// Get retrieves a session by ID, returning nil if expired or not found
func (ss *SessionStore) Get(sid string) *sessionEntry {
	ss.mu.RLock()
	entry, ok := ss.sessions[sid]
	ss.mu.RUnlock()
	if !ok {
		return nil
	}
	if time.Now().UnixMilli() > entry.expires {
		ss.mu.Lock()
		delete(ss.sessions, sid)
		ss.mu.Unlock()
		return nil
	}
	return entry
}

// Destroy removes a session
func (ss *SessionStore) Destroy(sid string) {
	ss.mu.Lock()
	delete(ss.sessions, sid)
	ss.mu.Unlock()
}

// DestroyAll clears all sessions (for security incidents)
func (ss *SessionStore) DestroyAll() {
	ss.mu.Lock()
	ss.sessions = make(map[string]*sessionEntry)
	ss.mu.Unlock()
}

// Count returns the number of active sessions
func (ss *SessionStore) Count() int {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return len(ss.sessions)
}

// Cleanup removes expired sessions
func (ss *SessionStore) Cleanup() {
	ss.mu.Lock()
	now := time.Now().UnixMilli()
	for sid, entry := range ss.sessions {
		if now > entry.expires {
			delete(ss.sessions, sid)
		}
	}
	ss.mu.Unlock()
}
