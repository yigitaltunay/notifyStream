package tracing

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func Setup(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	if strings.TrimSpace(serviceName) == "" {
		serviceName = "notifystream"
	}
	prop := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	otel.SetTextMapPropagator(prop)

	ep := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if ep == "" {
		ep = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	}
	if ep == "" {
		otel.SetTracerProvider(sdktrace.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
