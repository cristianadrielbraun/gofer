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
			redirectToLogin(w, r)
			return
		}

		session, err := m.GetSessionByToken(r.Context(), token)
		if err != nil {
			log.Printf("session lookup error: %v", err)
			redirectToLogin(w, r)
			return
		}
		if session == nil {
			ClearSessionCookie(w)
			redirectToLogin(w, r)
			return
		}

		user, err := m.GetUserByID(r.Context(), session.UserID)
		if err != nil {
			log.Printf("user lookup error: %v", err)
			redirectToLogin(w, r)
			return
		}
		if user == nil {
			ClearSessionCookie(w)
			redirectToLogin(w, r)
			return
		}

		ctx := ContextWithUser(r.Context(), user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		SetReturnToCookie(w, r.URL.RequestURI())
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
