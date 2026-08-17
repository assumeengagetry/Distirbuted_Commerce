package observability

import (
	"testing"

	"google.golang.org/grpc/stats"
)

func TestIdentityRPCFilterAllowsOnlyBoundedMethods(t *testing.T) {
	t.Parallel()
	for _, method := range []string{
		"/identity.v1.IdentityService/ValidateAccessToken",
		"/grpc.health.v1.Health/Check",
		"/grpc.health.v1.Health/List",
		"/grpc.health.v1.Health/Watch",
	} {
		if !IdentityRPCFilter(&stats.RPCTagInfo{FullMethodName: method}) {
			t.Errorf("IdentityRPCFilter(%q) = false", method)
		}
	}
	for _, method := range []string{"", "/attacker.Service/arbitrary", "/identity.v1.IdentityService/Unknown"} {
		if IdentityRPCFilter(&stats.RPCTagInfo{FullMethodName: method}) {
			t.Errorf("IdentityRPCFilter(%q) = true", method)
		}
	}
	if IdentityRPCFilter(nil) {
		t.Fatal("IdentityRPCFilter(nil) = true")
	}
}
