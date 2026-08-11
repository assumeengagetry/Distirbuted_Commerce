package grpctransport

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	identityv1 "github.com/assumeengagetry/distributed-commerce/internal/genproto/identity/v1"
)

func TestIdentityServerValidatesAccessToken(t *testing.T) {
	t.Parallel()
	principal := auth.Principal{UserID: uuid.New(), Role: "customer", TokenID: uuid.New()}
	parser := &stubAccessTokenParser{principal: principal}
	server, err := NewIdentityServer(parser, func() time.Time { return time.Unix(100, 0).UTC() })
	if err != nil {
		t.Fatalf("NewIdentityServer() error = %v", err)
	}
	response, err := server.ValidateAccessToken(t.Context(), &identityv1.ValidateAccessTokenRequest{AccessToken: "token"})
	if err != nil {
		t.Fatalf("ValidateAccessToken() error = %v", err)
	}
	if parser.captured != "token" {
		t.Fatalf("parsed token = %q", parser.captured)
	}
	if response.GetUserId() != principal.UserID.String() || response.GetTokenId() != principal.TokenID.String() ||
		response.GetRole() != identityv1.PrincipalRole_PRINCIPAL_ROLE_CUSTOMER {
		t.Fatalf("response = %+v", response)
	}
}

func TestIdentityServerSanitizesInvalidTokens(t *testing.T) {
	t.Parallel()
	server, err := NewIdentityServer(
		&stubAccessTokenParser{err: errors.New("private parser detail")}, time.Now,
	)
	if err != nil {
		t.Fatalf("NewIdentityServer() error = %v", err)
	}
	_, err = server.ValidateAccessToken(t.Context(), &identityv1.ValidateAccessTokenRequest{AccessToken: "secret-token"})
	if status.Code(err) != codes.Unauthenticated || status.Convert(err).Message() != "invalid access token" {
		t.Fatalf("ValidateAccessToken() error = %v", err)
	}
}

type stubAccessTokenParser struct {
	principal auth.Principal
	err       error
	captured  string
}

func (s *stubAccessTokenParser) ParseAccess(token string, _ time.Time) (auth.Principal, error) {
	s.captured = token
	return s.principal, s.err
}

var _ accessTokenParser = (*stubAccessTokenParser)(nil)

func TestIdentityServerRejectsInvalidRole(t *testing.T) {
	t.Parallel()
	server, err := NewIdentityServer(&stubAccessTokenParser{principal: auth.Principal{
		UserID: uuid.New(), Role: "owner", TokenID: uuid.New(),
	}}, time.Now)
	if err != nil {
		t.Fatalf("NewIdentityServer() error = %v", err)
	}
	_, err = server.ValidateAccessToken(context.Background(), &identityv1.ValidateAccessTokenRequest{AccessToken: "token"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ValidateAccessToken() code = %s", status.Code(err))
	}
}

func TestIdentityServerPreservesContextDeadlineCode(t *testing.T) {
	t.Parallel()
	server, err := NewIdentityServer(&stubAccessTokenParser{}, time.Now)
	if err != nil {
		t.Fatalf("NewIdentityServer() error = %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = server.ValidateAccessToken(ctx, &identityv1.ValidateAccessTokenRequest{})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("ValidateAccessToken() code = %s, want DeadlineExceeded", status.Code(err))
	}
}
