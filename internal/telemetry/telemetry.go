package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const instrumentationName = "github.com/lsm/dolmen"

type Tracing struct {
	tracer           trace.Tracer
	prop             propagation.TextMapPropagator
	includePrincipal bool
}

func New(tp trace.TracerProvider, prop propagation.TextMapPropagator, includePrincipal bool) *Tracing {
	if tp == nil {
		return nil
	}
	if _, off := tp.(noop.TracerProvider); off {
		return nil
	}
	if prop == nil {
		prop = propagation.NewCompositeTextMapPropagator()
	}
	return &Tracing{tracer: tp.Tracer(instrumentationName), prop: prop, includePrincipal: includePrincipal}
}

func (t *Tracing) On() bool { return t != nil }

type Provider struct {
	Tracing  *Tracing
	shutdown func(context.Context) error
}

func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

type exporterFactory func(ctx context.Context) (sdktrace.SpanExporter, error)

func Setup(ctx context.Context, getenv func(string) string, serviceVersion string) (*Provider, error) {
	return setup(ctx, getenv, serviceVersion, func(ctx context.Context) (sdktrace.SpanExporter, error) {
		return otlptracehttp.New(ctx)
	})
}

func setup(ctx context.Context, getenv func(string) string, serviceVersion string, newExporter exporterFactory) (*Provider, error) {
	on, err := exportEnabled(getenv)
	if err != nil || !on {
		return &Provider{}, err
	}
	for _, key := range []string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL"} {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			if v != "http/protobuf" {
				return nil, fmt.Errorf("%s=%q is not supported: dolmen exports traces over OTLP http/protobuf only (gRPC is left out to keep the binary small); unset it or set it to http/protobuf and point the endpoint at the collector's HTTP port, usually 4318", key, v)
			}
			break
		}
	}
	sampler, err := samplerFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	prop, err := propagatorFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	res, err := resourceFromEnv(ctx, getenv, serviceVersion)
	if err != nil {
		return nil, err
	}
	includePrincipal, err := boolEnv(getenv, "DOLMEN_OTEL_INCLUDE_PRINCIPAL")
	if err != nil {
		return nil, err
	}
	exp, err := newExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("OTLP trace exporter: %w (check OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_TRACES_ENDPOINT)", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sampler),
		sdktrace.WithResource(res),
	)
	return &Provider{Tracing: New(tp, prop, includePrincipal), shutdown: tp.Shutdown}, nil
}

func exportEnabled(getenv func(string) string) (bool, error) {
	disabled, err := boolEnv(getenv, "OTEL_SDK_DISABLED")
	if err != nil {
		return false, err
	}
	if disabled {
		return false, nil
	}
	switch v := strings.ToLower(strings.TrimSpace(getenv("OTEL_TRACES_EXPORTER"))); v {
	case "":
		return strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != "" || strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) != "", nil
	case "none":
		return false, nil
	case "otlp":
		return true, nil
	default:
		return false, fmt.Errorf("OTEL_TRACES_EXPORTER=%q is not supported: use otlp (with OTEL_EXPORTER_OTLP_ENDPOINT) or none", v)
	}
}

func boolEnv(getenv func(string) string, key string) (bool, error) {
	switch v := strings.ToLower(strings.TrimSpace(getenv(key))); v {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s=%q: use true or false", key, v)
	}
}

func samplerFromEnv(getenv func(string) string) (sdktrace.Sampler, error) {
	name := strings.ToLower(strings.TrimSpace(getenv("OTEL_TRACES_SAMPLER")))
	arg := strings.TrimSpace(getenv("OTEL_TRACES_SAMPLER_ARG"))
	ratio := func() (float64, error) {
		if arg == "" {
			return 1, nil
		}
		r, err := strconv.ParseFloat(arg, 64)
		if err != nil || r < 0 || r > 1 {
			return 0, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG=%q: use a sampling ratio between 0 and 1", arg)
		}
		return r, nil
	}
	switch name {
	case "", "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample()), nil
	case "always_on":
		return sdktrace.AlwaysSample(), nil
	case "always_off":
		return sdktrace.NeverSample(), nil
	case "traceidratio":
		r, err := ratio()
		if err != nil {
			return nil, err
		}
		return sdktrace.TraceIDRatioBased(r), nil
	case "parentbased_traceidratio":
		r, err := ratio()
		if err != nil {
			return nil, err
		}
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(r)), nil
	default:
		return nil, fmt.Errorf("OTEL_TRACES_SAMPLER=%q is not supported: use always_on, always_off, traceidratio, parentbased_always_on, parentbased_always_off or parentbased_traceidratio", name)
	}
}

func propagatorFromEnv(getenv func(string) string) (propagation.TextMapPropagator, error) {
	raw := strings.TrimSpace(getenv("OTEL_PROPAGATORS"))
	if raw == "" {
		raw = "tracecontext,baggage"
	}
	var props []propagation.TextMapPropagator
	for _, name := range strings.Split(raw, ",") {
		switch n := strings.ToLower(strings.TrimSpace(name)); n {
		case "tracecontext":
			props = append(props, propagation.TraceContext{})
		case "baggage":
			props = append(props, propagation.Baggage{})
		case "none", "":
		default:
			return nil, fmt.Errorf("OTEL_PROPAGATORS names %q, which is not supported: use tracecontext, baggage or none", n)
		}
	}
	return propagation.NewCompositeTextMapPropagator(props...), nil
}

func resourceFromEnv(ctx context.Context, getenv func(string) string, serviceVersion string) (*sdkresource.Resource, error) {
	attrs, err := parseResourceAttributes(getenv("OTEL_RESOURCE_ATTRIBUTES"))
	if err != nil {
		return nil, err
	}
	name := "dolmen"
	for _, kv := range attrs {
		if kv.Key == semconv.ServiceNameKey {
			name = kv.Value.AsString()
		}
	}
	if v := strings.TrimSpace(getenv("OTEL_SERVICE_NAME")); v != "" {
		name = v
	}
	attrs = append(attrs,
		semconv.ServiceName(name),
		semconv.ServiceVersion(serviceVersion),
		semconv.ServiceInstanceID(instanceID()),
	)
	res, err := sdkresource.New(ctx,
		sdkresource.WithSchemaURL(semconv.SchemaURL),
		sdkresource.WithTelemetrySDK(),
		sdkresource.WithHost(),
		sdkresource.WithProcessPID(),
		sdkresource.WithProcessRuntimeName(),
		sdkresource.WithProcessRuntimeVersion(),
		sdkresource.WithAttributes(attrs...),
	)
	if err != nil && !errors.Is(err, sdkresource.ErrPartialResource) && !errors.Is(err, sdkresource.ErrSchemaURLConflict) {
		return nil, fmt.Errorf("build the OpenTelemetry resource: %w", err)
	}
	if res == nil {
		res = sdkresource.NewWithAttributes(semconv.SchemaURL, attrs...)
	}
	return res, nil
}

func parseResourceAttributes(raw string) ([]attribute.KeyValue, error) {
	var out []attribute.KeyValue
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("OTEL_RESOURCE_ATTRIBUTES: entry %d is not key=value; separate entries with commas and percent-encode commas or equals signs inside values", len(out)+1)
		}
		val, err := url.PathUnescape(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("OTEL_RESOURCE_ATTRIBUTES: the value for %q has an invalid percent-encoding", k)
		}
		out = append(out, attribute.String(k, val))
	}
	return out, nil
}

func instanceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
