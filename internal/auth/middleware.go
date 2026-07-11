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
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		session, err := m.GetSessionByToken(r.Context(), token)
		if err != nil {
			log.Printf("session lookup error: %v", err)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if session == nil {
			ClearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		user, err := m.GetUserByID(r.Context(), session.UserID)
		if err != nil {
			log.Printf("user lookup error: %v", err)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if user == nil {
			ClearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		ctx := ContextWithUser(r.Context(), user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
