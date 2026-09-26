package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

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
	span  trace.Span
	ctx   context.Context
	inst  *instruments
	op    string
	start time.Time
}

func (t *Tracing) StartOp(ctx context.Context, op, requestID, principal string) (context.Context, *OpSpan) {
	if !t.On() {
		return ctx, nil
	}
	attrs := []attribute.KeyValue{OpNameKey.String(op)}
	if requestID != "" {
		attrs = append(attrs, RequestIDKey.String(clean(requestID, 128)))
	}
	if t.includePrincipal && principal != "" {
		attrs = append(attrs, PrincipalKey.String(clean(principal, maxNameAttr)))
	}
	ctx, span := t.tracer.Start(ctx, "dolmen.op "+op, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	t.inst.opStarted(ctx, op)
	return ctx, &OpSpan{span: span, ctx: ctx, inst: t.inst, op: op, start: time.Now()}
}

func (o *OpSpan) Recording() bool { return o != nil && o.span.IsRecording() }

func (o *OpSpan) SetScope(namespace, table string) {
	if !o.Recording() {
		return
	}
	if namespace != "" {
		o.span.SetAttributes(semconv.DBNamespace(clean(namespace, maxNameAttr)))
	}
	if table != "" {
		o.span.SetAttributes(TableKey.String(clean(table, maxNameAttr)))
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
	o.inst.opEnded(o.ctx, o.op, outcome, time.Since(o.start))
}

func PeekScope(body []byte) (namespace, table string) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", ""
	}
	var ns, tbl string
	var sawNS, sawTable bool
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", ""
		}
		key, ok := tok.(string)
		if !ok {
			return "", ""
		}
		if key != "namespace" && key != "table" {
			if skipValue(dec) != nil {
				return "", ""
			}
			continue
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return "", ""
		}
		if key == "namespace" {
			ns, sawNS = rawString(raw), true
		} else {
			tbl, sawTable = rawString(raw), true
		}
		if sawNS && sawTable {
			return ns, tbl
		}
	}
	if _, err := dec.Token(); err != nil {
		return "", ""
	}
	return ns, tbl
}

func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
