package telemetry

import (
	"net"
	"net/http"
	"strconv"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const RequestIDKey = attribute.Key("dolmen.request_id")

var knownMethods = map[string]bool{
	http.MethodConnect: true, http.MethodDelete: true, http.MethodGet: true, http.MethodHead: true,
	http.MethodOptions: true, http.MethodPatch: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodTrace: true, "QUERY": true,
}

type ServerOption int

const (
	EndAtHeaders ServerOption = iota + 1
)

func (t *Tracing) Server(route string, next http.Handler, opts ...ServerOption) http.Handler {
	if !t.On() {
		return next
	}
	endAtHeaders := false
	for _, o := range opts {
		if o == EndAtHeaders {
			endAtHeaders = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := t.prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		method := r.Method
		attrs := make([]attribute.KeyValue, 0, 12)
		if knownMethods[method] {
			attrs = append(attrs, semconv.HTTPRequestMethodKey.String(method))
		} else {
			method = "HTTP"
			attrs = append(attrs, semconv.HTTPRequestMethodKey.String("_OTHER"), semconv.HTTPRequestMethodOriginal(r.Method))
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		attrs = append(attrs,
			semconv.HTTPRoute(route),
			semconv.URLScheme(scheme),
			semconv.URLPath(r.URL.Path),
		)
		if host, port := splitHostPort(r.Host); host != "" {
			attrs = append(attrs, semconv.ServerAddress(host))
			if port > 0 {
				attrs = append(attrs, semconv.ServerPort(port))
			}
		}
		if host, _ := splitHostPort(r.RemoteAddr); host != "" {
			attrs = append(attrs, semconv.ClientAddress(host))
		}
		if ua := r.UserAgent(); ua != "" {
			attrs = append(attrs, semconv.UserAgentOriginal(ua))
		}
		switch {
		case r.ProtoMajor == 1 && r.ProtoMinor == 1:
			attrs = append(attrs, semconv.NetworkProtocolVersion("1.1"))
		case r.ProtoMajor == 2:
			attrs = append(attrs, semconv.NetworkProtocolVersion("2"))
		case r.ProtoMajor == 1 && r.ProtoMinor == 0:
			attrs = append(attrs, semconv.NetworkProtocolVersion("1.0"))
		}
		ctx, span := t.tracer.Start(ctx, method+" "+route, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		rw := &statusWriter{ResponseWriter: w, span: span, endAtHeaders: endAtHeaders}
		defer rw.finish()
		next.ServeHTTP(rw, r.WithContext(ctx))
	})
}

func splitHostPort(hostport string) (string, int) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, 0
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

type statusWriter struct {
	http.ResponseWriter
	span         trace.Span
	endAtHeaders bool
	status       int
	once         sync.Once
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
		if w.endAtHeaders {
			w.finish()
		}
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) finish() {
	w.once.Do(func() {
		status := w.status
		if status == 0 {
			status = http.StatusOK
		}
		w.span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		if id := w.ResponseWriter.Header().Get("X-Request-Id"); id != "" {
			w.span.SetAttributes(RequestIDKey.String(truncate(id, 128)))
		}
		if status >= http.StatusInternalServerError {
			w.span.SetAttributes(semconv.ErrorTypeKey.String(strconv.Itoa(status)))
			w.span.SetStatus(codes.Error, "")
		}
		w.span.End()
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
