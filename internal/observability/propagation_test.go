package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestPublicHTTPPropagatorDoesNotTrustExternalParent(t *testing.T) {
	t.Parallel()
	carrier := propagation.MapCarrier{
		"traceparent": "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01",
	}
	ctx := PublicHTTPPropagator().Extract(context.Background(), carrier)
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		t.Fatalf("public HTTP propagator retained external context: %v", spanContext)
	}
}
