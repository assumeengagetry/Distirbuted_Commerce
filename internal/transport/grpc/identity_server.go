package grpctransport

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	identityv1 "github.com/assumeengagetry/distributed-commerce/internal/genproto/identity/v1"
)

type accessTokenParser interface {
	ParseAccess(string, time.Time) (auth.Principal, error)
}

type IdentityServer struct {
	identityv1.UnimplementedIdentityServiceServer
	parser accessTokenParser
	now    func() time.Time
}

func NewIdentityServer(parser accessTokenParser, now func() time.Time) (*IdentityServer, error) {
	if parser == nil {
		return nil, fmt.Errorf("access token parser is required")
	}
	if now == nil {
		return nil, fmt.Errorf("clock is required")
	}
	return &IdentityServer{parser: parser, now: now}, nil
}

func (s *IdentityServer) ValidateAccessToken(
	ctx context.Context,
	request *identityv1.ValidateAccessTokenRequest,
) (*identityv1.ValidateAccessTokenResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if request == nil {
		return nil, status.Error(codes.Unauthenticated, "invalid access token")
	}
	principal, err := s.parser.ParseAccess(request.GetAccessToken(), s.now().UTC())
	if err != nil || principal.UserID == uuid.Nil || principal.TokenID == uuid.Nil {
		return nil, status.Error(codes.Unauthenticated, "invalid access token")
	}
	role := identityv1.PrincipalRole_PRINCIPAL_ROLE_UNSPECIFIED
	switch principal.Role {
	case "customer":
		role = identityv1.PrincipalRole_PRINCIPAL_ROLE_CUSTOMER
	case "admin":
		role = identityv1.PrincipalRole_PRINCIPAL_ROLE_ADMIN
	default:
		return nil, status.Error(codes.Unauthenticated, "invalid access token")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return &identityv1.ValidateAccessTokenResponse{
		UserId: principal.UserID.String(), Role: role, TokenId: principal.TokenID.String(),
	}, nil
}
