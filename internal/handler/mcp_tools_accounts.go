package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mailautodiscover "github.com/cristianadrielbraun/gofer/internal/mail/autodiscover"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type mcpServerSettings struct {
	IMAPHost     string `json:"imap_host,omitempty"`
	IMAPPort     int    `json:"imap_port,omitempty" jsonschema:"993 by default"`
	IMAPSecurity string `json:"imap_security,omitempty" jsonschema:"tls (SSL/TLS, the default) or starttls"`
	SMTPHost     string `json:"smtp_host,omitempty"`
	SMTPPort     int    `json:"smtp_port,omitempty" jsonschema:"465 by default"`
	SMTPSecurity string `json:"smtp_security,omitempty" jsonschema:"tls (SSL/TLS, the default) or starttls"`
	Username     string `json:"username,omitempty" jsonschema:"the sign-in name; the address by default"`
	Password     string `json:"password,omitempty"`
	SMTPUsername string `json:"smtp_username,omitempty" jsonschema:"only when SMTP signs in with another name than IMAP"`
	SMTPPassword string `json:"smtp_password,omitempty"`
}

type mcpDiscoverIn struct {
	Email string `json:"email"`
}

type mcpDiscoverOut struct {
	Candidates []mailautodiscover.Candidate `json:"candidates" jsonschema:"server settings found for the address, likeliest first"`
}

type mcpTestConnectionIn struct {
	Email    string `json:"email"`
	Provider string `json:"provider,omitempty" jsonschema:"imap (default) or jmap"`
	mcpServerSettings
	JMAPServer string `json:"jmap_server,omitempty" jsonschema:"for jmap: the server or its session URL; the address's domain by default"`
}

type mcpTestConnectionOut struct {
	OK      bool                          `json:"ok"`
	Results []models.ConnectionTestResult `json:"results,omitempty"`
	// Message is the JMAP test's outcome, as the add-account wizard shows it.
	Message string `json:"message,omitempty"`
}

type mcpAddAccountIn struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty" jsonschema:"the account's name in Gofer; the address by default"`
	Provider string `json:"provider,omitempty" jsonschema:"imap (default) or jmap"`
	mcpServerSettings
	JMAPServer string `json:"jmap_server,omitempty" jsonschema:"for jmap: the server or its session URL; the address's domain by default"`
	Color      string `json:"color,omitempty" jsonschema:"#rrggbb"`
	Contacts   bool   `json:"contacts,omitempty" jsonschema:"also sync contacts (CardDAV, found on the same server)"`
	Calendar   bool   `json:"calendar,omitempty" jsonschema:"also sync calendars (CalDAV, found on the same server)"`
	SkipTest   bool   `json:"skip_test,omitempty" jsonschema:"add it without first checking that it can sign in"`
}

type mcpAddAccountOut struct {
	Account  mcpAccount `json:"account"`
	Settings string     `json:"settings" jsonschema:"where the server settings came from"`
	Notes    []string   `json:"notes,omitempty"`
}

type mcpAccountIn struct {
	AccountID string `json:"account_id"`
}

type mcpUpdateAccountIn struct {
	AccountID  string `json:"account_id"`
	Name       string `json:"name,omitempty"`
	Email      string `json:"email,omitempty"`
	SenderName string `json:"sender_name,omitempty" jsonschema:"the name the account's own address sends under"`
	mcpServerSettings
	JMAPServer string `json:"jmap_server,omitempty" jsonschema:"for a jmap account: its server or session URL"`
}

type mcpSharedMailboxNameIn struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email" jsonschema:"the shared mailbox's address, from list_accounts"`
	Name      string `json:"name" jsonschema:"the name to show; empty for the server's"`
}

type mcpSharedMailboxIn struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email" jsonschema:"the shared mailbox's address, from list_accounts"`
	Enabled   bool   `json:"enabled" jsonschema:"true to use it, false to turn it off"`
}

type mcpServicesIn struct {
	AccountID  string `json:"account_id"`
	Mail       *bool  `json:"mail,omitempty"`
	Contacts   *bool  `json:"contacts,omitempty"`
	Calendar   *bool  `json:"calendar,omitempty"`
	CardDAVURL string `json:"carddav_url,omitempty" jsonschema:"where contacts are, when they are not found on their own"`
	CalDAVURL  string `json:"caldav_url,omitempty" jsonschema:"where calendars are, when they are not found on their own"`
}

type mcpColorIn struct {
	AccountID string `json:"account_id"`
	Color     string `json:"color" jsonschema:"#rrggbb"`
}

type mcpOrderIn struct {
	AccountIDs []string `json:"account_ids" jsonschema:"every account, in the order wanted"`
}

type mcpSyncIn struct {
	AccountID string `json:"account_id,omitempty" jsonschema:"one account; every account when empty"`
}

type mcpDeleteAccountIn struct {
	AccountID string `json:"account_id"`
	Confirm   bool   `json:"confirm,omitempty" jsonschema:"must be true: the account and all its mail in Gofer go away (the mail on the server stays)"`
}

func mcpTLS(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "starttls":
		return "starttls"
	case "", "tls", "ssl", "ssl/tls", "implicit":
		return "tls"
	default:
		return mode
	}
}

// form is the account form the web app posts for these settings.
func (s mcpServerSettings) form(email string) url.Values {
	username := strings.TrimSpace(s.Username)
	if username == "" {
		username = strings.TrimSpace(email)
	}
	imapPort, smtpPort := s.IMAPPort, s.SMTPPort
	if imapPort == 0 {
		imapPort = 993
	}
	if smtpPort == 0 {
		smtpPort = 465
	}
	return url.Values{
		"email_address": {strings.TrimSpace(email)},
		"imap_host":     {strings.TrimSpace(s.IMAPHost)}, "imap_port": {strconv.Itoa(imapPort)}, "imap_tls_mode": {mcpTLS(s.IMAPSecurity)},
		"smtp_host": {strings.TrimSpace(s.SMTPHost)}, "smtp_port": {strconv.Itoa(smtpPort)}, "smtp_tls_mode": {mcpTLS(s.SMTPSecurity)},
		"username": {username}, "password": {s.Password}, "auth_method": {"plain"},
		"smtp_username": {strings.TrimSpace(s.SMTPUsername)}, "smtp_password": {s.SMTPPassword},
	}
}

// mcpDiscover finds the server settings of an address, as the add-account
// wizard does.
func (h *Handler) mcpDiscover(ctx context.Context, email string) ([]mailautodiscover.Candidate, error) {
	resp, err := h.mcpInvoke(ctx, h.handleDiscoverAccount, mcpRequest{Path: "/api/accounts/discover", Form: url.Values{"email_address": {email}}})
	if err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	var out struct {
		Candidates []mailautodiscover.Candidate `json:"candidates"`
	}
	if err := resp.Decode(&out); err != nil {
		return nil, err
	}
	return out.Candidates, nil
}

func (h *Handler) mcpTestConnection(ctx context.Context, email string, s mcpServerSettings) mcpTestConnectionOut {
	f := s.form(email)
	port := func(key string) int { n, _ := strconv.Atoi(f.Get(key)); return n }
	cfg := &models.AccountConfig{
		Provider: "imap", AuthMethod: "plain", Username: f.Get("username"), SmtpUsername: f.Get("smtp_username"),
		IMAPHost: f.Get("imap_host"), IMAPPort: port("imap_port"), SMTPHost: f.Get("smtp_host"), SMTPPort: port("smtp_port"),
	}
	smtpPassword := s.Password
	if s.SMTPUsername != "" {
		smtpPassword = s.SMTPPassword
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out := mcpTestConnectionOut{Results: []models.ConnectionTestResult{
		h.testNewAccountService(ctx, "imap", cfg, f.Get("imap_tls_mode"), s.Password),
		h.testNewAccountService(ctx, "smtp", cfg, f.Get("smtp_tls_mode"), smtpPassword),
	}}
	out.OK = out.Results[0].Success && out.Results[1].Success
	return out
}

// mcpTestJMAPConnection tests a JMAP login as the add-account wizard does.
func (h *Handler) mcpTestJMAPConnection(ctx context.Context, in mcpTestConnectionIn) mcpTestConnectionOut {
	email := strings.TrimSpace(in.Email)
	form := url.Values{"email_address": {email}, "username": {firstNonEmptyString(in.Username, email)}, "password": {in.Password},
		"auth_method": {"plain"}, "provider": {providers.ProviderJMAP}, "mail_service": {providers.ProviderJMAP}, "jmap_server": {in.JMAPServer}}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resp, err := h.mcpInvoke(ctx, h.handleTestNewAccount, mcpRequest{Path: "/api/accounts/test", Form: form})
	if err != nil {
		return mcpTestConnectionOut{Message: err.Error()}
	}
	return mcpTestConnectionOut{
		OK:      resp.OK() && resp.Header.Get(accountConnectionTestResultHeader) == "ok",
		Message: mcpHTMLText(string(resp.Body)),
	}
}

// mcpHTMLText is the text of an HTML fragment on one line.
func mcpHTMLText(fragment string) string {
	return strings.Join(strings.Fields(signatureHTMLToText(fragment)), " ")
}

// mcpAccountView is one account as list_accounts shows it.
func (h *Handler) mcpAccountView(ctx context.Context, userID, accountID string) (mcpAccount, error) {
	accounts, err := h.mcpAccounts(ctx, userID, accountID)
	if err != nil {
		return mcpAccount{}, err
	}
	return h.mcpAccountDetails(ctx, accounts[0]), nil
}

func (h *Handler) mcpSetService(ctx context.Context, accountID, service string, enabled bool) error {
	resp, err := h.mcpInvoke(ctx, h.handleUpdateAccountService, mcpRequest{Path: "/api/accounts/" + accountID + "/services", PathValues: map[string]string{"id": accountID},
		Form: url.Values{"service": {service}, "enabled": {strconv.FormatBool(enabled)}}})
	if err != nil {
		return err
	}
	return resp.Err()
}

// mcpConnectContacts finds the account's address books on its server and
// syncs them all, signing in as the account.
func (h *Handler) mcpConnectContacts(ctx context.Context, accountID, carddavURL string) error {
	form := url.Values{"use_account_credentials": {"1"}}
	if carddavURL = strings.TrimSpace(carddavURL); carddavURL != "" {
		form.Set("base_url", carddavURL)
	} else {
		form.Set("autodiscover", "1")
	}
	resp, err := h.mcpInvoke(ctx, h.handleDiscoverAccountContactSync, mcpRequest{Path: "/api/accounts/" + accountID + "/contacts/sync/discover", PathValues: map[string]string{"id": accountID}, Form: form})
	if err != nil {
		return err
	}
	if err := resp.Err(); err != nil {
		return fmt.Errorf("finding the contacts server: %w", err)
	}
	var found struct {
		AddressBooks []models.ContactAddressBook `json:"address_books"`
	}
	if err := resp.Decode(&found); err != nil || len(found.AddressBooks) == 0 {
		return errors.New("no address books were found; give carddav_url")
	}
	save := url.Values{"use_account_credentials": {"1"}, "base_url": {firstNonEmptyString(carddavURL, found.AddressBooks[0].URL)}}
	for i, book := range found.AddressBooks {
		save.Add("addressbook_url", book.URL)
		save.Add("addressbook_name", book.Name)
		save.Add("addressbook_id", book.ID)
		if i == 0 || book.Default {
			save.Set("default_addressbook_url", book.URL)
		}
	}
	resp, err = h.mcpInvoke(ctx, h.handleSaveAccountContactSync, mcpRequest{Path: "/api/accounts/" + accountID + "/contacts/sync", PathValues: map[string]string{"id": accountID}, Form: save})
	if err != nil {
		return err
	}
	if err := resp.Err(); err != nil {
		return fmt.Errorf("saving the contacts server: %w", err)
	}
	return nil
}

// mcpConnectCalendars finds the account's calendars on its server; the
// ones found are synced once calendars are turned on.
func (h *Handler) mcpConnectCalendars(ctx context.Context, accountID, caldavURL string) error {
	form := url.Values{"use_account_credentials": {"1"}}
	if caldavURL = strings.TrimSpace(caldavURL); caldavURL != "" {
		form.Set("caldav_url", caldavURL)
	} else {
		form.Set("autodiscover", "1")
	}
	resp, err := h.mcpInvoke(ctx, h.handleDiscoverAccountCalendars, mcpRequest{Path: "/api/accounts/" + accountID + "/calendar/discover", PathValues: map[string]string{"id": accountID}, Form: form})
	if err != nil {
		return err
	}
	if err := resp.Err(); err != nil {
		return fmt.Errorf("finding the calendar server: %w", err)
	}
	data, err := h.accountStore.GetEditData(ctx, accountID)
	if err != nil {
		return err
	}
	if data.CalendarSync.SourceCount == 0 {
		return errors.New("no calendars were found; give caldav_url")
	}
	return nil
}

func (h *Handler) addMCPAccountTools(server *mcp.Server) {
	reads := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(true)}
	changes := &mcp.ToolAnnotations{OpenWorldHint: boolPtr(true), DestructiveHint: boolPtr(false)}
	destructive := &mcp.ToolAnnotations{OpenWorldHint: boolPtr(false), DestructiveHint: boolPtr(true)}

	mcp.AddTool(server, &mcp.Tool{Name: "discover_account_settings", Title: "Find server settings", Annotations: reads,
		Description: "Find the IMAP and SMTP servers of an email address, as the add-account wizard does."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpDiscoverIn) (*mcp.CallToolResult, mcpDiscoverOut, error) {
			ctx, _, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpDiscoverOut{}, err
			}
			candidates, err := h.mcpDiscover(ctx, strings.TrimSpace(in.Email))
			return nil, mcpDiscoverOut{Candidates: candidates}, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "test_account_connection", Title: "Test a mail login", Annotations: reads,
		Description: "Check that an address can sign in with these settings, without adding it: IMAP and SMTP, or a JMAP server (provider jmap)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpTestConnectionIn) (*mcp.CallToolResult, mcpTestConnectionOut, error) {
			ctx, _, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpTestConnectionOut{}, err
			}
			if strings.EqualFold(in.Provider, providers.ProviderJMAP) {
				if strings.TrimSpace(in.Email) == "" || in.Password == "" {
					return nil, mcpTestConnectionOut{}, errors.New("email and password are required")
				}
				return nil, h.mcpTestJMAPConnection(ctx, in), nil
			}
			if strings.TrimSpace(in.Email) == "" || in.IMAPHost == "" || in.SMTPHost == "" || in.Password == "" {
				return nil, mcpTestConnectionOut{}, errors.New("email, imap_host, smtp_host and password are required")
			}
			return nil, h.mcpTestConnection(ctx, in.Email, in.mcpServerSettings), nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "add_account", Title: "Add a mail account", Annotations: changes,
		Description: "Add an IMAP/SMTP (or JMAP) mail account. Without server settings they are found from the address. It is tested before it is added, then syncs; contacts and calendar can be turned on with it. Shared mailboxes the login can open are found on their own."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpAddAccountIn) (*mcp.CallToolResult, mcpAddAccountOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpAddAccountOut{}, err
			}
			email := strings.TrimSpace(in.Email)
			if email == "" || in.Password == "" {
				return nil, mcpAddAccountOut{}, errors.New("email and password are required")
			}
			out := mcpAddAccountOut{Settings: "given"}
			form := in.form(email)
			if strings.EqualFold(in.Provider, providers.ProviderJMAP) {
				form = url.Values{"email_address": {email}, "username": {firstNonEmptyString(in.Username, email)}, "password": {in.Password},
					"auth_method": {"plain"}, "provider": {providers.ProviderJMAP}, "mail_service": {providers.ProviderJMAP}, "jmap_server": {in.JMAPServer}}
			} else {
				if in.IMAPHost == "" || in.SMTPHost == "" {
					candidates, err := h.mcpDiscover(ctx, email)
					if err != nil {
						return nil, out, fmt.Errorf("the server settings could not be found (%v); give imap_host and smtp_host", err)
					}
					var found *mailautodiscover.Candidate
					for i := range candidates {
						if candidates[i].IMAPHost != "" && candidates[i].SMTPHost != "" && (candidates[i].Provider == "" || candidates[i].Provider == providers.ProviderIMAP) {
							found = &candidates[i]
							break
						}
					}
					if found == nil {
						return nil, out, errors.New("no IMAP and SMTP servers were found for this address; give imap_host and smtp_host")
					}
					settings := in.mcpServerSettings
					settings.IMAPHost, settings.IMAPPort, settings.IMAPSecurity = found.IMAPHost, found.IMAPPort, found.IMAPTLSMode
					settings.SMTPHost, settings.SMTPPort, settings.SMTPSecurity = found.SMTPHost, found.SMTPPort, found.SMTPTLSMode
					if settings.Username == "" {
						settings.Username = found.Username
					}
					in.mcpServerSettings = settings
					form = settings.form(email)
					out.Settings = "found (" + found.Source + "): " + found.IMAPHost + ", " + found.SMTPHost
				}
				if !in.SkipTest {
					if test := h.mcpTestConnection(ctx, email, in.mcpServerSettings); !test.OK {
						var problems []string
						for _, r := range test.Results {
							if !r.Success {
								problems = append(problems, strings.ToUpper(r.Service)+": "+firstNonEmptyString(r.Error, r.Message))
							}
						}
						return nil, out, fmt.Errorf("the account could not sign in, so it was not added: %s", strings.Join(problems, "; "))
					}
				}
			}
			form.Set("display_name", firstNonEmptyString(strings.TrimSpace(in.Name), email))

			before := map[string]bool{}
			if accounts, err := h.db.GetAccounts(ctx, userID); err == nil {
				for _, a := range accounts {
					before[a.ID] = true
				}
			}
			resp, err := h.mcpInvoke(ctx, h.handleCreateAccount, mcpRequest{Path: "/api/accounts", Form: form})
			if err != nil {
				return nil, out, err
			}
			accounts, err := h.db.GetAccounts(ctx, userID)
			if err != nil {
				return nil, out, err
			}
			accountID := ""
			for _, a := range accounts {
				if !before[a.ID] && strings.EqualFold(a.Email, email) {
					accountID = a.ID
				}
			}
			if accountID == "" {
				resp.Status = http.StatusBadRequest
				return nil, out, resp.Err()
			}
			if in.Color != "" {
				if err := h.mcpSetColor(ctx, accountID, in.Color); err != nil {
					out.Notes = append(out.Notes, "color: "+err.Error())
				}
			}
			if in.Contacts {
				if err := h.mcpConnectContacts(ctx, accountID, ""); err != nil {
					out.Notes = append(out.Notes, "contacts: "+err.Error())
				} else if err := h.mcpSetService(ctx, accountID, "contacts", true); err != nil {
					out.Notes = append(out.Notes, "contacts: "+err.Error())
				}
			}
			if in.Calendar {
				if err := h.mcpConnectCalendars(ctx, accountID, ""); err != nil {
					out.Notes = append(out.Notes, "calendar: "+err.Error())
				} else if err := h.mcpSetService(ctx, accountID, "calendar", true); err != nil {
					out.Notes = append(out.Notes, "calendar: "+err.Error())
				}
			}
			out.Account, err = h.mcpAccountView(ctx, userID, accountID)
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "update_account", Title: "Change an account", Annotations: changes,
		Description: "Change an account's name, address, sender name, server settings or password. Only what is given changes."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpUpdateAccountIn) (*mcp.CallToolResult, mcpAccount, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpAccount{}, err
			}
			if _, err := h.mcpAccounts(ctx, userID, in.AccountID); err != nil {
				return nil, mcpAccount{}, err
			}
			if in.SenderName != "" {
				resp, err := h.mcpInvoke(ctx, h.handleSaveAccountSenderName, mcpRequest{Path: "/api/accounts/" + in.AccountID + "/identities/sender-name", PathValues: map[string]string{"id": in.AccountID}, Form: url.Values{"sender_name": {in.SenderName}}})
				if err == nil {
					err = resp.Err()
				}
				if err != nil {
					return nil, mcpAccount{}, fmt.Errorf("sender name: %w", err)
				}
			}
			changed := in.Name != "" || in.Email != "" || in.mcpServerSettings != (mcpServerSettings{}) || in.JMAPServer != ""
			if changed {
				data, err := h.accountStore.GetEditData(ctx, in.AccountID)
				if err != nil {
					return nil, mcpAccount{}, err
				}
				pick := func(given, current string) string { return firstNonEmptyString(strings.TrimSpace(given), current) }
				pickPort := func(given, current int) int {
					if given > 0 {
						return given
					}
					return current
				}
				settings := mcpServerSettings{
					IMAPHost: pick(in.IMAPHost, data.IMAPHost), IMAPPort: pickPort(in.IMAPPort, data.IMAPPort), IMAPSecurity: pick(in.IMAPSecurity, data.IMAPTLSMode),
					SMTPHost: pick(in.SMTPHost, data.SMTPHost), SMTPPort: pickPort(in.SMTPPort, data.SMTPPort), SMTPSecurity: pick(in.SMTPSecurity, data.SMTPTLSMode),
					Username: pick(in.Username, data.Username), Password: in.Password,
					SMTPUsername: pick(in.SMTPUsername, data.SmtpUsername), SMTPPassword: in.SMTPPassword,
				}
				form := settings.form(pick(in.Email, data.EmailAddress))
				form.Set("display_name", pick(in.Name, data.DisplayName))
				form.Set("provider", data.Provider)
				form.Set("auth_method", firstNonEmptyString(data.AuthMethod, "plain"))
				form.Set("jmap_session_url", data.JMAPSessionURL)
				if in.JMAPServer != "" {
					form.Set("jmap_session_url", "")
					form.Set("jmap_server", in.JMAPServer)
				}
				form.Set("proxy", data.ProxyID)
				resp, err := h.mcpInvoke(ctx, h.handleUpdateAccount, mcpRequest{Path: "/api/accounts/" + in.AccountID + "/edit", PathValues: map[string]string{"id": in.AccountID}, Form: form})
				if err != nil {
					return nil, mcpAccount{}, err
				}
				if strings.Contains(string(resp.Body), "data-account-form-error") {
					resp.Status = http.StatusBadRequest
				}
				if err := resp.Err(); err != nil {
					return nil, mcpAccount{}, err
				}
			}
			view, err := h.mcpAccountView(ctx, userID, in.AccountID)
			return nil, view, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "set_shared_mailbox_enabled", Title: "Turn a shared mailbox on or off", Annotations: changes,
		Description: "Use one of the mailboxes an account can open besides its own (list_accounts shows them), or stop using it. One turned off is not synced, its folders are hidden and its From address is not offered; nothing changes on the server."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSharedMailboxIn) (*mcp.CallToolResult, mcpAccount, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpAccount{}, err
			}
			if _, err := h.mcpAccounts(ctx, userID, in.AccountID); err != nil {
				return nil, mcpAccount{}, err
			}
			err = h.db.SetSharedMailboxEnabled(ctx, in.AccountID, in.Email, in.Enabled)
			if errors.Is(err, storage.ErrSharedMailboxNotFound) {
				return nil, mcpAccount{}, errors.New("this account has no shared mailbox with that address (see list_accounts)")
			}
			if err != nil {
				return nil, mcpAccount{}, err
			}
			h.startSharedMailboxSync(ctx, in.AccountID)
			view, err := h.mcpAccountView(ctx, userID, in.AccountID)
			return nil, view, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "rename_shared_mailbox", Title: "Name a shared mailbox", Annotations: changes,
		Description: "Give one of an account's shared mailboxes the name Gofer shows for it, in the sidebar and elsewhere. An empty name goes back to the server's name, or one made from its address. Its From address keeps its own name (save_identity changes that)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSharedMailboxNameIn) (*mcp.CallToolResult, mcpAccount, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpAccount{}, err
			}
			if _, err := h.mcpAccounts(ctx, userID, in.AccountID); err != nil {
				return nil, mcpAccount{}, err
			}
			err = h.db.SetSharedMailboxName(ctx, in.AccountID, in.Email, in.Name)
			if errors.Is(err, storage.ErrSharedMailboxNotFound) {
				return nil, mcpAccount{}, errors.New("this account has no shared mailbox with that address (see list_accounts)")
			}
			if err != nil {
				return nil, mcpAccount{}, err
			}
			view, err := h.mcpAccountView(ctx, userID, in.AccountID)
			return nil, view, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "set_account_services", Title: "Turn mail, contacts or calendar on or off", Annotations: changes,
		Description: "Turn an account's mail, contacts or calendar sync on or off. Turning contacts or calendars on for the first time finds them on the account's server (CardDAV, CalDAV), signing in as the account."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpServicesIn) (*mcp.CallToolResult, mcpAccount, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpAccount{}, err
			}
			accounts, err := h.mcpAccounts(ctx, userID, in.AccountID)
			if err != nil {
				return nil, mcpAccount{}, err
			}
			account := accounts[0]
			if in.Mail != nil {
				if err := h.mcpSetService(ctx, account.ID, "email", *in.Mail); err != nil {
					return nil, mcpAccount{}, fmt.Errorf("mail: %w", err)
				}
			}
			if in.Contacts != nil {
				if *in.Contacts && !account.ContactSyncEnabled && account.Provider == providers.ProviderIMAP {
					data, err := h.accountStore.GetEditData(ctx, account.ID)
					if err != nil {
						return nil, mcpAccount{}, err
					}
					if len(data.ContactSync.AddressBooks) == 0 || in.CardDAVURL != "" {
						if err := h.mcpConnectContacts(ctx, account.ID, in.CardDAVURL); err != nil {
							return nil, mcpAccount{}, fmt.Errorf("contacts: %w", err)
						}
					}
				}
				if err := h.mcpSetService(ctx, account.ID, "contacts", *in.Contacts); err != nil {
					return nil, mcpAccount{}, fmt.Errorf("contacts: %w", err)
				}
			}
			if in.Calendar != nil {
				if *in.Calendar && !account.CalendarSyncEnabled && (account.Provider == providers.ProviderIMAP || account.Provider == providers.ProviderJMAP) {
					data, err := h.accountStore.GetEditData(ctx, account.ID)
					if err != nil {
						return nil, mcpAccount{}, err
					}
					if data.CalendarSync.SourceCount == 0 || in.CalDAVURL != "" {
						if err := h.mcpConnectCalendars(ctx, account.ID, in.CalDAVURL); err != nil {
							return nil, mcpAccount{}, fmt.Errorf("calendar: %w", err)
						}
					}
				}
				if err := h.mcpSetService(ctx, account.ID, "calendar", *in.Calendar); err != nil {
					return nil, mcpAccount{}, fmt.Errorf("calendar: %w", err)
				}
			}
			view, err := h.mcpAccountView(ctx, userID, account.ID)
			return nil, view, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "set_account_color", Title: "Set an account's color", Annotations: changes,
		Description: "Change the color that marks an account in the sidebar and message list."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpColorIn) (*mcp.CallToolResult, mcpAccount, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpAccount{}, err
			}
			if err := h.mcpSetColor(ctx, in.AccountID, in.Color); err != nil {
				return nil, mcpAccount{}, err
			}
			view, err := h.mcpAccountView(ctx, userID, in.AccountID)
			return nil, view, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "reorder_accounts", Title: "Order the accounts", Annotations: changes,
		Description: "Put the accounts in the sidebar in this order; name every account once."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpOrderIn) (*mcp.CallToolResult, mcpListAccountsOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpListAccountsOut{}, err
			}
			resp, err := h.mcpInvoke(ctx, h.handleAccountOrder, mcpRequest{Path: "/api/accounts/order", Form: url.Values{"account_id": in.AccountIDs}})
			if err != nil {
				return nil, mcpListAccountsOut{}, err
			}
			if err := resp.Err(); err != nil {
				return nil, mcpListAccountsOut{}, err
			}
			accounts, err := h.mcpAccounts(ctx, userID, "")
			out := mcpListAccountsOut{Accounts: []mcpAccount{}}
			for _, a := range accounts {
				out.Accounts = append(out.Accounts, mcpAccount{ID: a.ID, Name: a.Name, Email: a.Email, Provider: a.Provider})
			}
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sync_accounts", Title: "Sync now", Annotations: &mcp.ToolAnnotations{OpenWorldHint: boolPtr(true), IdempotentHint: true},
		Description: "Check the mail server for new mail and changes now, for one account or all; shared mailboxes are found again too. It runs in the background."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSyncIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			call := mcpRequest{Path: "/api/mail/sync"}
			handler := h.handleSyncMail
			if id := strings.TrimSpace(in.AccountID); id != "" {
				if _, err := h.mcpAccounts(ctx, userID, id); err != nil {
					return nil, mcpChangeOut{}, err
				}
				call = mcpRequest{Path: "/api/mail/sync/accounts/" + id, PathValues: map[string]string{"id": id}}
				handler = h.handleSyncMailAccount
			}
			resp, err := h.mcpInvoke(ctx, handler, call)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := resp.Err(); err != nil {
				return nil, mcpChangeOut{}, err
			}
			out := mcpChangeOut{Result: map[string]any{"started": true}}
			_ = resp.Decode(&out.Result)
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "remove_account", Title: "Remove an account", Annotations: destructive,
		Description: "Remove an account from Gofer with all it keeps here (its mail on the server stays). Needs confirm: true."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpDeleteAccountIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeAccounts)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if _, err := h.mcpAccounts(ctx, userID, in.AccountID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if !in.Confirm {
				return nil, mcpChangeOut{}, errors.New("removing an account cannot be undone: ask the user, then call again with confirm: true")
			}
			resp, err := h.mcpInvoke(ctx, h.handleDeleteAccount, mcpRequest{Method: http.MethodDelete, Path: "/api/accounts/" + in.AccountID, PathValues: map[string]string{"id": in.AccountID}})
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := resp.Err(); err != nil {
				return nil, mcpChangeOut{}, err
			}
			out := mcpChangeOut{Result: map[string]any{}}
			_ = resp.Decode(&out.Result)
			return nil, out, nil
		})
}

func (h *Handler) mcpSetColor(ctx context.Context, accountID, color string) error {
	resp, err := h.mcpInvoke(ctx, h.handleUpdateAccountColor, mcpRequest{Path: "/api/accounts/" + accountID + "/color", PathValues: map[string]string{"id": accountID}, Form: url.Values{"color": {color}}})
	if err != nil {
		return err
	}
	return resp.Err()
}
