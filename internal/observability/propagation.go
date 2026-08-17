package observability

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
)

type publicHTTPPropagator struct {
	traceContext propagation.TraceContext
}

func PublicHTTPPropagator() propagation.TextMapPropagator {
	return publicHTTPPropagator{traceContext: propagation.TraceContext{}}
}

func (p publicHTTPPropagator) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	p.traceContext.Inject(ctx, carrier)
}

func (p publicHTTPPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	return ctx
}

func (p publicHTTPPropagator) Fields() []string {
	return p.traceContext.Fields()
}
