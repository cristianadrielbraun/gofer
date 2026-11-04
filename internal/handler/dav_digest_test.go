package handler

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The worked example from RFC 7616, section 3.9.1.
func TestDAVDigestAuthorizationMatchesRFC7616(t *testing.T) {
	challenge := davDigestChallenge{
		realm:  "http-auth@example.org",
		nonce:  "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		opaque: "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS",
		qop:    true,
	}
	cnonce := "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ"
	for algorithm, want := range map[string]string{
		"MD5":     `response="8ca523f5e9506fed4657c9700eebdbec"`,
		"SHA-256": `response="753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"`,
	} {
		challenge.algorithm = algorithm
		header, err := davDigestAuthorization(challenge, "GET", "/dir/index.html", "Mufasa", "Circle of Life", 1, cnonce)
		if err != nil || !strings.Contains(header, want) || !strings.Contains(header, "nc=00000001") {
			t.Errorf("%s: header = %q, err = %v, want %s", algorithm, header, err, want)
		}
	}
}

func TestDAVDigestChallengePrefersSHA256AndSkipsOtherSchemes(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("WWW-Authenticate", `Basic realm="files", Digest realm="dav", nonce="n1", algorithm=MD5, qop="auth,auth-int"`)
	resp.Header.Add("WWW-Authenticate", `Digest realm="dav", nonce="n2", algorithm=SHA-256, qop="auth", opaque="o\"q"`)
	challenge, ok := davDigestChallengeFrom(resp)
	if !ok || challenge.nonce != "n2" || challenge.algorithm != "SHA-256" || !challenge.qop || challenge.opaque != `o"q` {
		t.Fatalf("challenge = %+v, %v", challenge, ok)
	}

	resp.Header.Set("WWW-Authenticate", `Digest realm="dav", nonce="n3", qop="auth-int"`)
	if _, ok := davDigestChallengeFrom(resp); ok {
		t.Fatal("a challenge that only offers auth-int was answered")
	}
	resp.Header.Set("WWW-Authenticate", `Basic realm="dav"`)
	if _, ok := davDigestChallengeFrom(resp); ok {
		t.Fatal("a Basic challenge was taken for Digest")
	}
}

// digestOnlyServer accepts only Digest auth, like Baïkal's default setup.
type digestOnlyServer struct {
	mu       sync.Mutex
	nonce    string
	requests []string // the scheme of each request's Authorization header
	bodies   []string // bodies of accepted requests
}

func (s *digestOnlyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	auth := r.Header.Get("Authorization")
	scheme, _, _ := strings.Cut(auth, " ")
	s.requests = append(s.requests, scheme)
	challenge := func(stale bool) {
		value := `Digest realm="BaikalDAV", nonce="` + s.nonce + `", qop="auth", algorithm="MD5"`
		if stale {
			value += ", stale=TRUE"
		}
		w.Header().Set("WWW-Authenticate", value)
		w.WriteHeader(http.StatusUnauthorized)
	}
	if scheme != "Digest" {
		challenge(false)
		return
	}
	params := davDigestChallenges(auth)
	if len(params) != 1 {
		challenge(false)
		return
	}
	p := params[0]
	if p["nonce"] != s.nonce {
		challenge(true)
		return
	}
	h := func(value string) string { sum := md5.Sum([]byte(value)); return hex.EncodeToString(sum[:]) }
	ha1 := h(p["username"] + ":BaikalDAV:right password")
	want := h(ha1 + ":" + p["nonce"] + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + h(r.Method+":"+p["uri"]))
	if p["response"] != want || p["uri"] != r.URL.RequestURI() {
		challenge(false)
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.bodies = append(s.bodies, string(body))
	w.WriteHeader(http.StatusMultiStatus)
}

func (s *digestOnlyServer) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	got := s.requests
	s.requests = nil
	return got
}

func digestRequest(t *testing.T, url, password, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "PROPFIND", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("lena", password)
	resp, err := davDigestRoundTrip(http.DefaultTransport, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func TestDAVRequestsAnswerADigestChallenge(t *testing.T) {
	server := &digestOnlyServer{nonce: "first"}
	ts := httptest.NewServer(server)
	defer ts.Close()

	// The first request offers Basic, is challenged, and is sent again with
	// its body.
	resp := digestRequest(t, ts.URL+"/dav.php/calendars/lena/?depth=1", "right password", "<propfind/>")
	if got := server.take(); resp.StatusCode != http.StatusMultiStatus || strings.Join(got, ",") != "Basic,Digest" || server.bodies[0] != "<propfind/>" {
		t.Fatalf("first request: status %d, requests %v, bodies %q", resp.StatusCode, got, server.bodies)
	}

	// Later requests answer the remembered challenge at once, without the password.
	resp = digestRequest(t, ts.URL+"/dav.php/calendars/lena/personal/", "right password", "<report/>")
	if got := server.take(); resp.StatusCode != http.StatusMultiStatus || strings.Join(got, ",") != "Digest" || server.bodies[1] != "<report/>" {
		t.Fatalf("second request: status %d, requests %v, bodies %q", resp.StatusCode, got, server.bodies)
	}

	// A stale nonce is answered again with the new one.
	server.mu.Lock()
	server.nonce = "second"
	server.mu.Unlock()
	resp = digestRequest(t, ts.URL+"/dav.php/", "right password", "<propfind/>")
	if got := server.take(); resp.StatusCode != http.StatusMultiStatus || strings.Join(got, ",") != "Digest,Digest" {
		t.Fatalf("stale nonce: status %d, requests %v", resp.StatusCode, got)
	}

	// A wrong password ends in the server's 401, and the challenge is forgotten
	// so the next request starts over.
	resp = digestRequest(t, ts.URL+"/dav.php/", "wrong password", "")
	if got := server.take(); resp.StatusCode != http.StatusUnauthorized || strings.Join(got, ",") != "Digest,Digest" {
		t.Fatalf("wrong password: status %d, requests %v", resp.StatusCode, got)
	}
	resp = digestRequest(t, ts.URL+"/dav.php/", "right password", "")
	if got := server.take(); resp.StatusCode != http.StatusMultiStatus || strings.Join(got, ",") != "Basic,Digest" {
		t.Fatalf("after a wrong password: status %d, requests %v", resp.StatusCode, got)
	}
}

// A server that takes Basic auth sees exactly one request, as before.
func TestDAVRequestsKeepBasicAuthWhenItWorks(t *testing.T) {
	var schemes []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, _, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		schemes = append(schemes, scheme)
		if _, _, ok := r.BasicAuth(); !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="dav"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
	}))
	defer ts.Close()
	if resp := digestRequest(t, ts.URL+"/", "secret", "<propfind/>"); resp.StatusCode != http.StatusMultiStatus || strings.Join(schemes, ",") != "Basic" {
		t.Fatalf("status %d, requests %v", resp.StatusCode, schemes)
	}
}
