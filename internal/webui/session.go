package webui

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/reorx/hookploy/internal/token"
	"github.com/reorx/hookploy/internal/webui/views"
)

const (
	sessionCookie = "hookploy_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// sessionStore keeps sessions in process memory: a main restart logs
// everyone out, which is acceptable for the single-admin DevOps use case.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // id → expiry
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]time.Time{}}
}

func (ss *sessionStore) create() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	id := hex.EncodeToString(b[:])
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.sessions[id] = time.Now().Add(sessionTTL)
	return id
}

func (ss *sessionStore) valid(id string) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	exp, ok := ss.sessions[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(ss.sessions, id)
		return false
	}
	return true
}

func (ss *sessionStore) delete(id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.sessions, id)
}

// expireAll rewrites every session's expiry (test hook).
func (ss *sessionStore) expireAll(to time.Time) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for id := range ss.sessions {
		ss.sessions[id] = to
	}
}

// SessionValid reports whether the request carries a live session cookie.
// Injected into httpapi.Server.SessionOK so the admin middleware accepts the
// UI session on GET/HEAD.
func (s *Server) SessionValid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return s.sessions.valid(c.Value)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	plain := r.PostFormValue("token")
	rec, err := s.Store.LookupToken(token.Hash(plain))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if plain == "" || rec == nil || rec.Kind != string(token.KindAdmin) {
		s.loginFailed(w, r)
		return
	}
	id := s.sessions.create()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// loginFailed re-renders the login page with an error, status 401.
func (s *Server) loginFailed(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusUnauthorized, views.Login("invalid admin token"))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

// fragmentAuth guards fragment routes: they are fetched by JS, so an
// unauthenticated request gets a plain 401 instead of a redirect (whose
// login-page body would end up injected into the DOM).
func (s *Server) fragmentAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.SessionValid(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// requireSession guards page routes: unauthenticated visitors are sent to
// the login page.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.SessionValid(r) {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}
