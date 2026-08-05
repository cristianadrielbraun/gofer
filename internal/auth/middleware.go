package auth

import (
	"context"
	"log"
	"net/http"
	"strings"
)

func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.config.Enabled {
			defaultUser := m.GetDefaultUser()
			ctx := ContextWithUser(r.Context(), defaultUser)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		path := r.URL.Path

		if isPublicPath(path) {
			next.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}

		token := GetSessionToken(r)
		if token == "" {
			m.redirectToLogin(w, r)
			return
		}

		session, err := m.GetSessionByToken(r.Context(), token)
		if err != nil {
			log.Printf("session lookup error: %v", err)
			m.redirectToLogin(w, r)
			return
		}
		if session == nil {
			ClearSessionCookie(w, m.config.SecureCookies)
			m.redirectToLogin(w, r)
			return
		}

		user, err := m.GetUserByID(r.Context(), session.UserID)
		if err != nil {
			log.Printf("user lookup error: %v", err)
			m.redirectToLogin(w, r)
			return
		}
		if user == nil {
			ClearSessionCookie(w, m.config.SecureCookies)
			m.redirectToLogin(w, r)
			return
		}
		if !user.Status.AllowsAuthentication() {
			ClearSessionCookie(w, m.config.SecureCookies)
			m.redirectToLogin(w, r)
			return
		}
		if user.AuthVersion != session.AuthVersion {
			ClearSessionCookie(w, m.config.SecureCookies)
			m.redirectToLogin(w, r)
			return
		}

		ctx := contextWithSessionCSRF(ContextWithSession(ContextWithUser(r.Context(), user), session), token)
		r = r.WithContext(ctx)
		if requiresSessionCSRF(r) && !validSessionCSRF(r) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *Manager) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		SetReturnToCookie(w, r.URL.RequestURI(), m.config.SecureCookies)
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func isPublicPath(path string) bool {
	public := []string{"/login", "/auth/google", "/auth/google/callback"}
	for _, p := range public {
		if path == p {
			return true
		}
	}
	return false
}

func GetCurrentUser(ctx context.Context) *User {
	return UserFromContext(ctx)
}

func GetCurrentSession(ctx context.Context) *Session {
	return SessionFromContext(ctx)
}
