package handler

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"
)

// DAV requests carry the account's user name and password as Basic auth. Some
// servers, Baïkal by default among them, accept only Digest auth: they answer
// with a 401 that carries a Digest challenge, and the request is sent again
// with an answer to it (RFC 7616). Once a server has asked for Digest, later
// requests to it answer the remembered challenge straight away and no longer
// send the password itself. Nobody has to change an account for this.

// davDigestMaxServers caps the remembered challenges. Each is a few short
// strings; the cap only stops a long-running instance from growing without end.
const davDigestMaxServers = 1024

type davDigestChallenge struct {
	realm, nonce, opaque string
	algorithm            string // MD5, MD5-sess, SHA-256 or SHA-256-sess
	qop                  bool   // qop=auth was offered
	userhash             bool
}

type davDigestState struct {
	challenge davDigestChallenge
	count     uint32
}

var davDigestCache = struct {
	sync.Mutex
	servers map[string]*davDigestState
}{servers: map[string]*davDigestState{}}

// davDigestKey identifies one protection space: a server and a user on it.
func davDigestKey(req *http.Request, username string) string {
	return strings.ToLower(req.URL.Scheme) + "://" + strings.ToLower(req.URL.Host) + "\x00" + username
}

// davDigestRoundTrip sends req through next, answering a Digest challenge when
// the server asks for one.
func davDigestRoundTrip(next http.RoundTripper, req *http.Request) (*http.Response, error) {
	username, password, ok := req.BasicAuth()
	if !ok {
		return next.RoundTrip(req)
	}
	key := davDigestKey(req, username)
	// The answer is computed before sending, so a request that needs its body
	// again can't be sent with a known challenge unless the body can be read twice.
	if state := davDigestRemembered(key); state != nil {
		digest, err := davDigestRequest(req, state.challenge, state.count, username, password)
		if err != nil {
			return next.RoundTrip(req)
		}
		if req.Body != nil {
			_ = req.Body.Close()
		}
		resp, err := next.RoundTrip(digest)
		if err != nil || resp.StatusCode != http.StatusUnauthorized {
			return resp, err
		}
		// The nonce went stale, or the server changed its mind; answer afresh.
		challenge, found := davDigestChallengeFrom(resp)
		if !found {
			davDigestForget(key)
			return resp, nil
		}
		return davDigestRetry(next, req, resp, key, challenge, username, password)
	}
	resp, err := next.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	challenge, found := davDigestChallengeFrom(resp)
	if !found {
		return resp, nil
	}
	return davDigestRetry(next, req, resp, key, challenge, username, password)
}

// davDigestRetry sends req again with an answer to challenge, in place of the
// 401 it got. The challenge is remembered only when the answer was accepted, so
// a wrong password doesn't keep a server on Digest it may not want.
func davDigestRetry(next http.RoundTripper, req *http.Request, unauthorized *http.Response, key string, challenge davDigestChallenge, username, password string) (*http.Response, error) {
	digest, err := davDigestRequest(req, challenge, 1, username, password)
	if err != nil {
		return unauthorized, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(unauthorized.Body, 64<<10))
	_ = unauthorized.Body.Close()
	resp, err := next.RoundTrip(digest)
	switch {
	case err != nil:
	case resp.StatusCode == http.StatusUnauthorized:
		davDigestForget(key)
	default:
		davDigestRemember(key, challenge)
	}
	return resp, err
}

func davDigestRemembered(key string) *davDigestState {
	davDigestCache.Lock()
	defer davDigestCache.Unlock()
	state := davDigestCache.servers[key]
	if state == nil {
		return nil
	}
	// Each use of a nonce carries the next count (nc).
	state.count++
	return &davDigestState{challenge: state.challenge, count: state.count}
}

func davDigestRemember(key string, challenge davDigestChallenge) {
	davDigestCache.Lock()
	defer davDigestCache.Unlock()
	if _, known := davDigestCache.servers[key]; !known && len(davDigestCache.servers) >= davDigestMaxServers {
		for other := range davDigestCache.servers {
			delete(davDigestCache.servers, other)
			break
		}
	}
	// The retry that proved the challenge used nc=1.
	davDigestCache.servers[key] = &davDigestState{challenge: challenge, count: 1}
}

func davDigestForget(key string) {
	davDigestCache.Lock()
	defer davDigestCache.Unlock()
	delete(davDigestCache.servers, key)
}

// davDigestRequest copies req with an answer to challenge in place of its
// Basic credentials. It fails when the body can't be sent again.
func davDigestRequest(req *http.Request, challenge davDigestChallenge, count uint32, username, password string) (*http.Request, error) {
	digest := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return nil, fmt.Errorf("request body can't be sent again")
		}
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		digest.Body = body
	}
	header, err := davDigestAuthorization(challenge, req.Method, req.URL.RequestURI(), username, password, count, "")
	if err != nil {
		return nil, err
	}
	digest.Header.Set("Authorization", header)
	return digest, nil
}

// davDigestAuthorization answers challenge for one request. cnonce is chosen
// at random when empty; tests pass a fixed one.
func davDigestAuthorization(challenge davDigestChallenge, method, uri, username, password string, count uint32, cnonce string) (string, error) {
	var newHash func() hash.Hash
	algorithm := strings.ToUpper(challenge.algorithm)
	switch strings.TrimSuffix(algorithm, "-SESS") {
	case "", "MD5":
		newHash = md5.New
	case "SHA-256":
		newHash = sha256.New
	default:
		return "", fmt.Errorf("unsupported Digest algorithm %q", challenge.algorithm)
	}
	h := func(parts ...string) string {
		sum := newHash()
		_, _ = io.WriteString(sum, strings.Join(parts, ":"))
		return hex.EncodeToString(sum.Sum(nil))
	}
	if cnonce == "" {
		cnonce = rand.Text()
	}
	nc := fmt.Sprintf("%08x", count)
	ha1 := h(username, challenge.realm, password)
	if strings.HasSuffix(algorithm, "-SESS") {
		ha1 = h(ha1, challenge.nonce, cnonce)
	}
	ha2 := h(method, uri)
	var response string
	if challenge.qop {
		response = h(ha1, challenge.nonce, nc, cnonce, "auth", ha2)
	} else {
		response = h(ha1, challenge.nonce, ha2)
	}
	name := username
	if challenge.userhash {
		name = h(username, challenge.realm)
	}
	var b strings.Builder
	b.WriteString("Digest username=" + davDigestQuote(name))
	b.WriteString(", realm=" + davDigestQuote(challenge.realm))
	b.WriteString(", nonce=" + davDigestQuote(challenge.nonce))
	b.WriteString(", uri=" + davDigestQuote(uri))
	if challenge.algorithm != "" {
		b.WriteString(", algorithm=" + challenge.algorithm)
	}
	b.WriteString(", response=" + davDigestQuote(response))
	if challenge.opaque != "" {
		b.WriteString(", opaque=" + davDigestQuote(challenge.opaque))
	}
	if challenge.qop {
		b.WriteString(", qop=auth, nc=" + nc + ", cnonce=" + davDigestQuote(cnonce))
	}
	if challenge.userhash {
		b.WriteString(", userhash=true")
	}
	return b.String(), nil
}

func davDigestQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// davDigestChallengeFrom picks the Digest challenge to answer from a 401,
// preferring SHA-256 over MD5. A challenge that offers only qop=auth-int, or
// an algorithm Gofer doesn't know, is skipped.
func davDigestChallengeFrom(resp *http.Response) (davDigestChallenge, bool) {
	var best davDigestChallenge
	found := false
	for _, value := range resp.Header.Values("Www-Authenticate") {
		for _, params := range davDigestChallenges(value) {
			challenge := davDigestChallenge{
				realm:     params["realm"],
				nonce:     params["nonce"],
				opaque:    params["opaque"],
				algorithm: params["algorithm"],
				userhash:  strings.EqualFold(params["userhash"], "true"),
			}
			if challenge.nonce == "" {
				continue
			}
			if qop, offered := params["qop"]; offered {
				for _, option := range strings.Split(qop, ",") {
					if strings.EqualFold(strings.TrimSpace(option), "auth") {
						challenge.qop = true
					}
				}
				if !challenge.qop {
					continue
				}
			}
			algorithm := strings.ToUpper(challenge.algorithm)
			if algorithm != "" && algorithm != "MD5" && algorithm != "MD5-SESS" && algorithm != "SHA-256" && algorithm != "SHA-256-SESS" {
				continue
			}
			if !found || strings.HasPrefix(algorithm, "SHA-256") {
				best, found = challenge, true
			}
		}
	}
	return best, found
}

// davDigestChallenges reads the Digest challenges in one WWW-Authenticate
// value, which may also list other schemes, as a map of parameters each.
func davDigestChallenges(value string) []map[string]string {
	var challenges []map[string]string
	var current map[string]string
	for value = strings.TrimSpace(value); value != ""; {
		// A token not followed by "=" starts a new challenge.
		end := strings.IndexAny(value, " =,")
		if end == -1 {
			end = len(value)
		}
		token := value[:end]
		rest := strings.TrimLeft(value[end:], " ")
		if token == "" {
			value = strings.TrimLeft(value[1:], " ")
			continue
		}
		if !strings.HasPrefix(rest, "=") {
			if current != nil {
				challenges = append(challenges, current)
			}
			current = nil
			if strings.EqualFold(token, "Digest") {
				current = map[string]string{}
			}
			value = strings.TrimLeft(strings.TrimPrefix(rest, ","), " ")
			continue
		}
		rest = strings.TrimLeft(rest[1:], " ")
		var param string
		if strings.HasPrefix(rest, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(rest); i++ {
				c := rest[i]
				if c == '\\' && i+1 < len(rest) {
					i++
					b.WriteByte(rest[i])
					continue
				}
				if c == '"' {
					i++
					break
				}
				b.WriteByte(c)
			}
			param, rest = b.String(), rest[i:]
		} else {
			stop := strings.IndexAny(rest, ", ")
			if stop == -1 {
				stop = len(rest)
			}
			param, rest = rest[:stop], rest[stop:]
		}
		if current != nil {
			current[strings.ToLower(token)] = param
		}
		value = strings.TrimLeft(strings.TrimLeft(rest, " "), ",")
		value = strings.TrimLeft(value, " ")
	}
	if current != nil {
		challenges = append(challenges, current)
	}
	return challenges
}
