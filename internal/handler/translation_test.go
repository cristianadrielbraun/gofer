package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/translation"
)

type fakeTextBatchProvider struct{}

func (fakeTextBatchProvider) TranslateTexts(ctx context.Context, sourceLanguage, targetLanguage string, texts []string) (translation.TextsResult, error) {
	out := make([]string, len(texts))
	for i, text := range texts {
		switch strings.TrimSpace(text) {
		case "Hello":
			out[i] = "Hola"
		case "world":
			out[i] = "mundo"
		default:
			out[i] = text
		}
	}
	return translation.TextsResult{
		Provider:         "fake",
		Texts:            out,
		DetectedLanguage: "en",
		TargetLanguage:   targetLanguage,
	}, nil
}

func TestTranslateHTMLTextNodesPreservesStructure(t *testing.T) {
	input := `<div class="message"><p>Hello <strong>world</strong></p><img src="/logo.png" alt="Logo"></div>`

	_, translated, err := translateHTMLTextNodes(context.Background(), fakeTextBatchProvider{}, "auto", "es", input)
	if err != nil {
		t.Fatalf("translateHTMLTextNodes() error = %v", err)
	}

	for _, want := range []string{`class="message"`, `<strong>mundo</strong>`, `src="/logo.png"`, `alt="Logo"`} {
		if !strings.Contains(translated, want) {
			t.Fatalf("translated html = %q, want to contain %q", translated, want)
		}
	}
	for _, notWant := range []string{"Hello", ">world<"} {
		if strings.Contains(translated, notWant) {
			t.Fatalf("translated html = %q, did not want %q", translated, notWant)
		}
	}
}
