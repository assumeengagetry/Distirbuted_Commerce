package observability

import "google.golang.org/grpc/stats"

func IdentityRPCFilter(info *stats.RPCTagInfo) bool {
	if info == nil {
		return false
	}
	switch info.FullMethodName {
	case "/identity.v1.IdentityService/ValidateAccessToken",
		"/grpc.health.v1.Health/Check",
		"/grpc.health.v1.Health/List",
		"/grpc.health.v1.Health/Watch":
		return true
	default:
		return false
	}
}
