package tracing

import (
	"context"
	"strings"
	"testing"
)

func TestSetup_noEndpoint_noop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	shutdown, err := Setup(context.Background(), "my-service")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if shutdown == nil {
		t.Fatal("nil shutdown")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestSetup_emptyServiceName_default(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	_, err := Setup(context.Background(), "   ")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
}

func TestSetup_invalidURL(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "://nope")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	_, err := Setup(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSetup_missingHost(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	_, err := Setup(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetup_badScheme(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "ftp://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	_, err := Setup(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("unexpected error: %v", err)
	}
}
