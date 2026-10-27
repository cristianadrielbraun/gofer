package handler

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type accountServerNote struct {
	// Level is "blocked" when Gofer will refuse the server, "approved" when an
	// exception lets it through.
	Level    string `json:"level"`
	Text     string `json:"text"`
	Link     string `json:"link,omitempty"`
	LinkText string `json:"link_text,omitempty"`
}

// handleAccountServerCheck tells the account form, while the person is still
// filling it in, whether Gofer will refuse the server they entered: one on a
// private network, or an unencrypted connection, without a mail security
// exception. Calendar and contacts servers are checked from their address.
// It only looks up the server's name and never connects to it.
func (h *Handler) handleAccountServerCheck(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	protocol := strings.ToLower(strings.TrimSpace(query.Get("protocol")))
	host := normalizePrivateTargetHost(query.Get("host"))
	port, portErr := strconv.Atoi(strings.TrimSpace(query.Get("port")))
	if address := strings.TrimSpace(query.Get("url")); address != "" {
		protocol, host, port, portErr = davServerEndpoint(address)
	}
	notes := []accountServerNote{}
	if (protocol == "imap" || protocol == "smtp" || protocol == "http" || protocol == "https") && validPrivateTargetHost(host) && portErr == nil && port >= 1 && port <= 65535 {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		notes = h.accountServerNotes(ctx, protocol, host, port, strings.ToLower(strings.TrimSpace(query.Get("tls_mode"))))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"notes": notes})
}

// davServerEndpoint returns the scheme, host and port Gofer connects to for a
// calendar or contacts server address.
func davServerEndpoint(address string) (string, string, int, error) {
	parsed, err := url.Parse(address)
	if err != nil {
		return "", "", 0, err
	}
	scheme := strings.ToLower(parsed.Scheme)
	port := 443
	if scheme == "http" {
		port = 80
	}
	if text := parsed.Port(); text != "" {
		if port, err = strconv.Atoi(text); err != nil {
			return "", "", 0, err
		}
	}
	return scheme, normalizePrivateTargetHost(parsed.Hostname()), port, nil
}

func (h *Handler) accountServerNotes(ctx context.Context, protocol, host string, port int, tlsMode string) []accountServerNote {
	label := strings.ToUpper(protocol)
	endpoint := net.JoinHostPort(host, strconv.Itoa(port))
	notes := []accountServerNote{}
	blocked := func(text, exception string) accountServerNote {
		note := accountServerNote{Level: "blocked", Text: text}
		if link := h.mailSecuritySettingsURL(); link != "" {
			note.Text += " Add " + exception + " for " + endpoint + " under Mail security."
			note.Link, note.LinkText = link, "Open Mail security"
		} else {
			note.Text += " Ask your Gofer administrator to add " + exception + " for " + endpoint + "."
		}
		return note
	}

	// No exception lets in a server the allowed mail servers leave out.
	if err := h.checkMailServer(ctx, host); storage.IsMailAllowlistError(err) {
		return append(notes, accountServerNote{Level: "blocked", Text: err.Error()})
	} else if err != nil {
		log.Printf("account server check: allowed mail servers for %s: %v", endpoint, err)
	}

	// A name that doesn't resolve yet gets no note: the person may still be
	// typing it, and the connection test reports it if it stays unknown.
	if private, err := netguard.IsPrivateHost(ctx, host); err == nil && private {
		allowed, err := h.db.IsPrivateTargetAllowed(ctx, protocol, host, port)
		switch {
		case err != nil:
			log.Printf("account server check: private target exception for %s %s: %v", protocol, endpoint, err)
		case allowed:
			notes = append(notes, accountServerNote{Level: "approved", Text: "This server is on a private network and is approved under Mail security."})
		default:
			notes = append(notes, blocked("This server is on a private network, so Gofer won't connect to it yet.", "an "+label+" private target exception"))
		}
	}
	if tlsMode == "plaintext" {
		allowed, err := h.db.IsPlaintextTransportAllowed(ctx, protocol, host, port)
		switch {
		case err != nil:
			log.Printf("account server check: plaintext exception for %s %s: %v", protocol, endpoint, err)
		case allowed:
			notes = append(notes, accountServerNote{Level: "approved", Text: "Unencrypted " + label + " to this server is approved under Mail security."})
		default:
			notes = append(notes, blocked("Gofer won't connect to this server without encryption yet.", "an unencrypted "+label+" exception"))
		}
	}
	return notes
}

// mailSecuritySettingsURL is where the current person can approve mail
// security exceptions, or "" when only an administrator can.
func (h *Handler) mailSecuritySettingsURL() string {
	if h.ownerManagesMailSecurity() {
		return "/settings/security#mail-security"
	}
	return ""
}
