package telemetry

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	OpNameKey    = attribute.Key("dolmen.op.name")
	OpOutcomeKey = attribute.Key("dolmen.op.outcome")
	TableKey     = attribute.Key("dolmen.table")
	PrincipalKey = semconv.EnduserIDKey
)

const maxNameAttr = 256

type OpSpan struct {
	span trace.Span
}

func (t *Tracing) StartOp(ctx context.Context, op, requestID, principal string) (context.Context, *OpSpan) {
	if !t.On() {
		return ctx, nil
	}
	attrs := []attribute.KeyValue{OpNameKey.String(op)}
	if requestID != "" {
		attrs = append(attrs, RequestIDKey.String(truncate(requestID, 128)))
	}
	if t.includePrincipal && principal != "" {
		attrs = append(attrs, PrincipalKey.String(truncate(principal, maxNameAttr)))
	}
	ctx, span := t.tracer.Start(ctx, "dolmen.op "+op, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	return ctx, &OpSpan{span: span}
}

func (o *OpSpan) Recording() bool { return o != nil && o.span.IsRecording() }

func (o *OpSpan) SetScope(namespace, table string) {
	if !o.Recording() {
		return
	}
	if namespace != "" {
		o.span.SetAttributes(semconv.DBNamespace(truncate(namespace, maxNameAttr)))
	}
	if table != "" {
		o.span.SetAttributes(TableKey.String(truncate(table, maxNameAttr)))
	}
}

func (o *OpSpan) End(outcome string) {
	if o == nil {
		return
	}
	if outcome == "" {
		outcome = "ok"
	}
	o.span.SetAttributes(OpOutcomeKey.String(outcome))
	if outcome != "ok" {
		o.span.SetAttributes(semconv.ErrorTypeKey.String(outcome))
		o.span.SetStatus(codes.Error, outcome)
	}
	o.span.End()
}

func PeekScope(body []byte) (namespace, table string) {
	var probe struct {
		Namespace json.RawMessage `json:"namespace"`
		Table     json.RawMessage `json:"table"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return "", ""
	}
	return rawString(probe.Namespace), rawString(probe.Table)
}

func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
