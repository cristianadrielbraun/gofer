package httpguard

// MCPPath serves Gofer's MCP server to AI clients. They prove who they are
// with a bearer token in the Authorization header, never a cookie, so a
// request from another site carries nothing a browser would add, and the
// same-origin check that guards the cookie-signed pages does not apply.
const MCPPath = "/mcp"

// The OAuth 2.1 endpoints that let an MCP client get a token. Like the MCP
// server, they are called by the client's own servers, carry no cookie, and
// prove themselves with what they post (PKCE, codes, tokens), never a
// session. The authorization page itself, OAuthAuthorizePath, is a page the
// signed-in user approves on, and goes through sign-in like any other.
const (
	OAuthProtectedResourcePath = "/.well-known/oauth-protected-resource"
	OAuthServerMetadataPath    = "/.well-known/oauth-authorization-server"
	OAuthRegisterPath          = "/oauth/register"
	OAuthTokenPath             = "/oauth/token"
	OAuthRevokePath            = "/oauth/revoke"
	OAuthAuthorizePath         = "/oauth/authorize"
)

// isTokenAuthenticated reports whether a path is signed with a bearer token
// or OAuth credentials rather than the session cookie.
func isTokenAuthenticated(path string) bool {
	switch path {
	case MCPPath, OAuthServerMetadataPath, OAuthRegisterPath, OAuthTokenPath, OAuthRevokePath:
		return true
	}
	return path == OAuthProtectedResourcePath || path == OAuthProtectedResourcePath+MCPPath ||
		path == OAuthServerMetadataPath+MCPPath
}

// IsTokenAuthenticated is isTokenAuthenticated for the sign-in middleware,
// which leaves these paths to their own token check.
func IsTokenAuthenticated(path string) bool { return isTokenAuthenticated(path) }

// MCPInvocationHeader marks a request an MCP tool makes to one of Gofer's
// own handlers inside the process (handler/mcp_invoke.go). It never comes
// from the network: those requests are not served by the HTTP stack.
const MCPInvocationHeader = "X-Gofer-MCP-Invocation"
