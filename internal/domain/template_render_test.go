package domain

import (
	"testing"
)

func TestRenderTemplateBody_substitution(t *testing.T) {
	out, err := RenderTemplateBody("Hi {{name}}", []byte(`{"name":"Ada"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "Hi Ada" {
		t.Fatalf("got %q", out)
	}
}

func TestRenderTemplateBody_noPlaceholders(t *testing.T) {
	out, err := RenderTemplateBody("plain", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "plain" {
		t.Fatalf("got %q", out)
	}
}
