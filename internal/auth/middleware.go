package auth

import (
	"context"
	"log"
	"net/http"
	"strings"
)

func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if m.IsPersonal() {
			if path == "/admin" || strings.HasPrefix(path, "/admin/") || strings.HasPrefix(path, "/api/admin/") || path == "/account/enroll" || strings.HasPrefix(path, "/account/enroll/") || path == "/account/recover" || (strings.HasPrefix(path, "/setup/") && path != "/setup/owner") {
				http.NotFound(w, r)
				return
			}
			state, err := m.SetupState(r.Context())
			if err != nil {
				http.Error(w, "Unable to read setup state", http.StatusServiceUnavailable)
				return
			}
			if !state.Initialized && path != "/setup" && path != "/setup/owner" && !strings.HasPrefix(path, "/assets/") {
				w.Header().Set("Cache-Control", "no-store")
				if r.Method == http.MethodGet && !strings.HasPrefix(path, "/api/") {
					http.Redirect(w, r, "/setup", http.StatusSeeOther)
				} else {
					http.Error(w, "Personal setup required", http.StatusForbidden)
				}
				return
			}
		}
		if path == "/setup" || path == "/setup/owner" || path == "/setup/password" || path == "/setup/mfa" || path == "/setup/recovery" || path == "/setup/review" {
			next.ServeHTTP(w, r)
			return
		}
		if !m.config.Enabled {
			defaultUser := m.GetDefaultUser()
			ctx := ContextWithUser(r.Context(), defaultUser)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

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
			m.rejectUnauthenticated(w, r)
			return
		}

		session, err := m.GetSessionByToken(r.Context(), token)
		if err != nil {
			log.Printf("session lookup error: %v", err)
			m.rejectUnauthenticated(w, r)
			return
		}
		if session == nil {
			ClearSessionCookie(w, m.config.SecureCookies)
			m.rejectUnauthenticated(w, r)
			return
		}

		user, err := m.GetUserByID(r.Context(), session.UserID)
		if err != nil {
			log.Printf("user lookup error: %v", err)
			m.rejectUnauthenticated(w, r)
			return
		}
		if user == nil {
			ClearSessionCookie(w, m.config.SecureCookies)
			m.rejectUnauthenticated(w, r)
			return
		}
		if !user.Status.AllowsAuthentication() {
			ClearSessionCookie(w, m.config.SecureCookies)
			m.rejectUnauthenticated(w, r)
			return
		}
		if user.AuthVersion != session.AuthVersion {
			ClearSessionCookie(w, m.config.SecureCookies)
			m.rejectUnauthenticated(w, r)
			return
		}
		if user.IsManagement() && !user.IsAdmin {
			ClearSessionCookie(w, m.config.SecureCookies)
			m.rejectUnauthenticated(w, r)
			return
		}

		ctx := contextWithSessionCSRF(ContextWithSession(ContextWithUser(r.Context(), user), session), token)
		r = r.WithContext(ctx)
		if m.enforceUserSurface(w, r, user) {
			return
		}
		if session.PasswordChangeRequired && path != RequiredPasswordChangePath && path != "/auth/logout" {
			w.Header().Set("Cache-Control", "no-store")
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", RequiredPasswordChangePath)
				w.WriteHeader(http.StatusForbidden)
			} else if r.Method != http.MethodGet || strings.HasPrefix(path, "/api/") || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
				http.Error(w, "password change required", http.StatusForbidden)
			} else {
				http.Redirect(w, r, RequiredPasswordChangePath, http.StatusSeeOther)
			}
			return
		}
		if requiresSessionCSRF(r) {
			r.Body = http.MaxBytesReader(w, r.Body, sessionCSRFFormMaximumBytes)
			if !validSessionCSRF(r) {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (m *Manager) enforceUserSurface(w http.ResponseWriter, r *http.Request, user *User) bool {
	path := r.URL.Path
	managementRoute := path == "/admin" || strings.HasPrefix(path, "/admin/") || strings.HasPrefix(path, "/api/admin/")
	sharedSecurityRoute := path == "/auth/logout" || path == "/settings/security" || strings.HasPrefix(path, "/settings/security/")

	if user.IsManagement() {
		allowed := managementRoute || sharedSecurityRoute
		if !allowed {
			m.rejectWrongSurface(w, r, "/admin")
			return true
		}
		if path == "/settings/security" && r.Method == http.MethodGet {
			destination := "/admin/account/security"
			if r.URL.RawQuery != "" {
				destination += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, destination, http.StatusSeeOther)
			return true
		}
		return false
	}
	if managementRoute {
		m.rejectWrongSurface(w, r, "/")
		return true
	}
	return false
}

func (m *Manager) rejectWrongSurface(w http.ResponseWriter, r *http.Request, destination string) {
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("{\"error\":\"account_surface_forbidden\"}\n"))
		return
	}
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", destination)
		http.Error(w, "account surface forbidden", http.StatusForbidden)
		return
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}

func (m *Manager) rejectUnauthenticated(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "HX-Request")
	loginPath := "/login"
	if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") || strings.HasPrefix(r.URL.Path, "/api/admin/") {
		loginPath = "/admin/login"
	}
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", loginPath)
		if r.Method == http.MethodGet {
			SetReturnToCookie(w, r.URL.RequestURI(), m.config.SecureCookies)
		}
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/api/events" || r.URL.Path == "/api/admin/events" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("{\"error\":\"authentication_required\"}\n"))
		return
	}
	if r.Method == http.MethodGet {
		SetReturnToCookie(w, r.URL.RequestURI(), m.config.SecureCookies)
	}
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}

func isHTMXRequest(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("HX-Request")), "true")
}

func isPublicPath(path string) bool {
	public := []string{
		"/login", "/admin/login", "/login/passkey/start", "/login/passkey/finish", "/login/mfa", "/login/mfa/recovery", "/login/recovery/mfa", "/login/recovery/codes",
		"/login/mfa/enroll", "/login/mfa/enroll/codes",
		"/setup", "/setup/owner", "/setup/password", "/setup/mfa", "/setup/recovery", "/setup/review",
		"/account/enroll", "/account/enroll/google", "/account/enroll/complete",
		"/account/redeem", "/account/redeem/complete", "/account/recover",
		"/auth/google", "/auth/google/login/callback", "/auth/microsoft", "/auth/microsoft/login/callback", "/auth/oidc", "/auth/oidc/callback", "/sw.js",
	}
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
