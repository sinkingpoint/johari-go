package johari

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
)

const INSTRUMENTATION_NAME = "github.com/johari/sinkingpoint"

type JohariConfig struct {
	ServiceName         string
	CollectorURL        string
	Debug               bool
	SamplingRate        float64
	Propagator          propagation.TextMapPropagator
	ExtraAllowedHeaders []string
}

var globalTracer trace.Tracer
var globalConfig JohariConfig

type ContextKey string

const requestSpanKey = ContextKey("request_span")

func init() {
	globalTracer = trace.NewNoopTracerProvider().Tracer(INSTRUMENTATION_NAME)
}

func InitTracing(config JohariConfig) {
	var sampler tracesdk.Sampler
	if config.SamplingRate >= 1 {
		sampler = tracesdk.AlwaysSample()
	} else if config.SamplingRate <= 0 {
		sampler = tracesdk.NeverSample()
	} else {
		sampler = tracesdk.TraceIDRatioBased(config.SamplingRate)
	}

	if config.Propagator == nil {
		config.Propagator = b3.B3{}
	}

	otel.SetTextMapPropagator(config.Propagator)

	var exporter tracesdk.SpanExporter
	var err error
	if config.Debug {
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
	} else {
		exporter, err = jaeger.New(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(config.CollectorURL)))
	}

	if err != nil {
		log.Fatalf("Failed to initialise tracing")
	}

	tp := tracesdk.NewTracerProvider(
		tracesdk.WithBatcher(exporter),
		tracesdk.WithSampler(sampler),
		tracesdk.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(config.ServiceName),
		)),
	)

	otel.SetTracerProvider(tp)

	globalTracer = tp.Tracer("github.com/sinkingpoint/johari")
	globalConfig = config
}

type johariMuxWrapper struct {
	backend http.Handler
}

func isAllowedHeader(name string) bool {
	normalisedName := strings.ToLower(name)
	for _, allowed := range []string{
		"accept", "accept-encoding", "accept-language", "cache-control",
		"connection", "host", "referer", "te", "useragent", "cf-cache-status",
		"cf-ray", "content-encoding", "content-type", "date", "expect-ct",
		"server", "vary",
	} {
		if normalisedName == allowed {
			return true
		}
	}

	if globalConfig.ExtraAllowedHeaders != nil {
		for _, allowed := range globalConfig.ExtraAllowedHeaders {
			if normalisedName == allowed {
				return true
			}
		}
	}

	return false
}

func populateSpanFromRequest(spanName string, r *http.Request) (context.Context, trace.Span) {
	var span trace.Span
	var ctx context.Context
	if r.Context().Value(requestSpanKey) != nil {
		parentSpanCtx := r.Context().Value(requestSpanKey).(context.Context)
		ctx, span = globalTracer.Start(parentSpanCtx, spanName)
	} else {
		ctx, span = globalTracer.Start(r.Context(), spanName)
		*r = *r.WithContext(context.WithValue(r.Context(), requestSpanKey, ctx))
	}

	span.SetAttributes(attribute.String("http.request.method", r.Method))
	span.SetAttributes(attribute.String("http.request.request_uri", r.URL.RequestURI()))
	span.SetAttributes(attribute.String("http.request.host", r.URL.Host))

	for k, v := range r.Header {
		if !isAllowedHeader(k) {
			continue
		}

		name := "http.request." + k

		span.SetAttributes(attribute.String(name, strings.Join(v, ", ")))
	}

	return ctx, span
}

func populateSpanFromResponse(span trace.Span, r *http.Response) {
	span.SetAttributes(attribute.Int("http.response.status_code", r.StatusCode))
	for k, v := range r.Header {
		if !isAllowedHeader(k) {
			continue
		}

		name := "http.response." + k

		span.SetAttributes(attribute.String(name, strings.Join(v, ", ")))
	}
}

func (j johariMuxWrapper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, span := populateSpanFromRequest(fmt.Sprintf("%s %s", r.Method, r.RequestURI), r)
	defer span.End()

	responseWriter := &johariResponseWriter{w, http.StatusOK, 0}
	j.backend.ServeHTTP(responseWriter, r)

	span.SetAttributes(attribute.Int("http.response_code", responseWriter.statusCode))
	span.SetAttributes(attribute.Int("http.response_size", responseWriter.length))
}

type johariResponseWriter struct {
	http.ResponseWriter
	statusCode int
	length     int
}

func (j *johariResponseWriter) WriteHeader(code int) {
	j.statusCode = code
	j.ResponseWriter.WriteHeader(code)
}

func (j *johariResponseWriter) Write(b []byte) (n int, err error) {
	n, err = j.ResponseWriter.Write(b)
	j.length += n

	return n, err
}

type johariHTTPTransport struct {
	backingTransport http.RoundTripper
}

func (j johariHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	_, span := populateSpanFromRequest(fmt.Sprintf("Send %s %s", r.Method, r.URL.String()), r)
	defer span.End()

	resp, err := j.backingTransport.RoundTrip(r)

	if err != nil {
		span.RecordError(err)
	} else {
		populateSpanFromResponse(span, resp)
	}

	return resp, err
}

func extractParentContext(ctx context.Context) context.Context {
	if ctx.Value(requestSpanKey) != nil {
		return ctx.Value(requestSpanKey).(context.Context)
	} else {
		return nil
	}
}

func NewHTTPServerWrapper(muxer http.Handler) johariMuxWrapper {
	return johariMuxWrapper{
		backend: muxer,
	}
}

func NewHTTPClientWrapper(client *http.Client) *http.Client {
	var transport http.RoundTripper

	if client.Transport != nil {
		transport = client.Transport
	} else {
		transport = http.DefaultTransport
	}

	_, ok := client.Transport.(johariHTTPTransport)

	if !ok {
		client.Transport = johariHTTPTransport{
			backingTransport: transport,
		}
	}

	return client
}

func NewChildRequest(ctx context.Context, method, url string, body io.ReadCloser) (*http.Request, error) {
	req, err := http.NewRequest(
		method,
		url,
		body,
	)

	if pctx := extractParentContext(ctx); pctx != nil {
		req = req.WithContext(pctx)
	} else {
		req = req.WithContext(ctx)
	}

	return req, err
}

func NewChildSpan(ctx context.Context, spanName string) (context.Context, trace.Span) {
	if pctx := extractParentContext(ctx); pctx != nil {
		return globalTracer.Start(pctx, spanName)
	} else {
		return globalTracer.Start(ctx, spanName)
	}
}
