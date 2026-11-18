package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/httpguard"
)

// mcpRequest is one call an MCP tool makes to one of Gofer's own HTTP
// handlers, so that a tool does exactly what the same action in the web app
// does, with the same checks. It runs in the process, as the tool's user
// (the context mcpBegin returned); nothing goes over the network.
type mcpRequest struct {
	Method string
	Path   string
	// PathValues fill the handler's {name} wildcards.
	PathValues map[string]string
	// One of JSON, Form or Multipart is the body.
	JSON      any
	Form      url.Values
	Multipart *mcpMultipart
}

type mcpMultipart struct {
	Fields   url.Values
	FileName string
	Field    string
	Content  []byte
}

// mcpResponse is what the handler answered.
type mcpResponse struct {
	Status int
	Body   []byte
	Header http.Header
}

// OK reports a 2xx answer.
func (r mcpResponse) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Decode reads a JSON answer into v.
func (r mcpResponse) Decode(v any) error { return json.Unmarshal(r.Body, v) }

var htmlTags = regexp.MustCompile(`<[^>]*>`)

// Err is the handler's refusal as an error a client can read: its JSON
// "error", or its text without markup.
func (r mcpResponse) Err() error {
	if r.OK() {
		return nil
	}
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	text := ""
	if json.Unmarshal(r.Body, &payload) == nil {
		text = strings.TrimSpace(payload.Error + " " + payload.Message)
	}
	if text == "" {
		text = strings.Join(strings.Fields(htmlTags.ReplaceAllString(string(r.Body), " ")), " ")
	}
	if text == "" {
		text = http.StatusText(r.Status)
	}
	if len(text) > 600 {
		text = text[:600] + "…"
	}
	return errors.New(text)
}

func (h *Handler) mcpInvoke(ctx context.Context, handler http.HandlerFunc, in mcpRequest) (mcpResponse, error) {
	var body io.Reader = http.NoBody
	contentType := ""
	switch {
	case in.JSON != nil:
		raw, err := json.Marshal(in.JSON)
		if err != nil {
			return mcpResponse{}, err
		}
		body, contentType = bytes.NewReader(raw), "application/json"
	case in.Form != nil:
		body, contentType = strings.NewReader(in.Form.Encode()), "application/x-www-form-urlencoded"
	case in.Multipart != nil:
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for key, values := range in.Multipart.Fields {
			for _, v := range values {
				_ = mw.WriteField(key, v)
			}
		}
		part, err := mw.CreateFormFile(in.Multipart.Field, in.Multipart.FileName)
		if err != nil {
			return mcpResponse{}, err
		}
		if _, err := part.Write(in.Multipart.Content); err != nil {
			return mcpResponse{}, err
		}
		if err := mw.Close(); err != nil {
			return mcpResponse{}, err
		}
		body, contentType = &buf, mw.FormDataContentType()
	}
	method := in.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, in.Path, body)
	if err != nil {
		return mcpResponse{}, fmt.Errorf("build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(httpguard.MCPInvocationHeader, "1")
	for name, value := range in.PathValues {
		req.SetPathValue(name, value)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return mcpResponse{Status: rec.Code, Body: rec.Body.Bytes(), Header: rec.Header()}, nil
}
