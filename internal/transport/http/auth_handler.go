package httptransport

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/user"
)

type UserService interface {
	Register(ctx context.Context, req user.RegisterRequest) (user.AuthResult, error)
	Login(ctx context.Context, req user.LoginRequest) (user.AuthResult, error)
	Refresh(ctx context.Context, refreshToken string) (user.AuthResult, error)
	Logout(ctx context.Context, refreshToken string) error
	GetProfile(ctx context.Context, userID uuid.UUID) (user.Profile, error)
}

type authHandler struct {
	service UserService
	logger  *slog.Logger
}

type registerBody struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type loginBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshBody struct {
	RefreshToken string `json:"refresh_token"`
}

type userResponse struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        user.Role `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type accountResponse struct {
	ID        uuid.UUID `json:"id"`
	Currency  string    `json:"currency"`
	Balance   int64     `json:"balance"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type tokenResponse struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	TokenType        string    `json:"token_type"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

type authResponse struct {
	User    userResponse    `json:"user"`
	Account accountResponse `json:"account"`
	Tokens  tokenResponse   `json:"tokens"`
}

type profileResponse struct {
	User    userResponse    `json:"user"`
	Account accountResponse `json:"account"`
}

func (h authHandler) register(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var body registerBody
	if err := decodeJSON(c, &body); err != nil {
		writeDecodeError(c, err)
		return
	}
	result, err := h.service.Register(c.Request.Context(), user.RegisterRequest{
		Email: body.Email, Password: body.Password, DisplayName: body.DisplayName,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, authResultResponse(result))
}

func (h authHandler) login(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var body loginBody
	if err := decodeJSON(c, &body); err != nil {
		writeDecodeError(c, err)
		return
	}
	result, err := h.service.Login(c.Request.Context(), user.LoginRequest{Email: body.Email, Password: body.Password})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, authResultResponse(result))
}

func (h authHandler) refresh(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var body refreshBody
	if err := decodeJSON(c, &body); err != nil {
		writeDecodeError(c, err)
		return
	}
	result, err := h.service.Refresh(c.Request.Context(), body.RefreshToken)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, authResultResponse(result))
}

func (h authHandler) logout(c *gin.Context) {
	var body refreshBody
	if err := decodeJSON(c, &body); err != nil {
		writeDecodeError(c, err)
		return
	}
	if err := h.service.Logout(c.Request.Context(), body.RefreshToken); err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h authHandler) me(c *gin.Context) {
	principal, ok := auth.PrincipalFromContext(c.Request.Context())
	if !ok {
		writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	profile, err := h.service.GetProfile(c.Request.Context(), principal.UserID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, profileResultResponse(profile))
}

func (h authHandler) writeServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, user.ErrInvalidEmail):
		writeAPIError(c, http.StatusBadRequest, "INVALID_EMAIL", "invalid email address")
	case errors.Is(err, user.ErrInvalidPassword):
		writeAPIError(c, http.StatusBadRequest, "INVALID_PASSWORD", err.Error())
	case errors.Is(err, user.ErrInvalidDisplayName):
		writeAPIError(c, http.StatusBadRequest, "INVALID_DISPLAY_NAME", err.Error())
	case errors.Is(err, user.ErrEmailAlreadyExists):
		writeAPIError(c, http.StatusConflict, "EMAIL_ALREADY_EXISTS", "email already registered")
	case errors.Is(err, user.ErrInvalidCredentials):
		writeAPIError(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "invalid email or password")
	case errors.Is(err, user.ErrInvalidRefreshToken):
		writeAPIError(c, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "invalid refresh token")
	case errors.Is(err, user.ErrUserDisabled):
		writeAPIError(c, http.StatusUnauthorized, "ACCOUNT_DISABLED", "account is disabled")
	case errors.Is(err, user.ErrUserNotFound):
		writeAPIError(c, http.StatusNotFound, "USER_NOT_FOUND", "user not found")
	case errors.Is(err, user.ErrTemporarilyUnavailable):
		c.Header("Retry-After", "1")
		writeAPIError(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "service temporarily unavailable")
	default:
		h.logger.ErrorContext(
			c.Request.Context(),
			"request failed",
			slog.String("request_id", requestIDFromContext(c)),
			slog.Any("error", err),
		)
		writeAPIError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
	}
}

func authResultResponse(result user.AuthResult) authResponse {
	profile := profileResultResponse(result.Profile)
	return authResponse{
		User:    profile.User,
		Account: profile.Account,
		Tokens: tokenResponse{
			AccessToken: result.Tokens.AccessToken, RefreshToken: result.Tokens.RefreshToken, TokenType: "Bearer",
			AccessExpiresAt: result.Tokens.AccessExpiresAt, RefreshExpiresAt: result.Tokens.RefreshExpiresAt,
		},
	}
}

func profileResultResponse(profile user.Profile) profileResponse {
	return profileResponse{
		User: userResponse{
			ID: profile.User.ID, Email: profile.User.Email, DisplayName: profile.User.DisplayName, Role: profile.User.Role,
			CreatedAt: profile.User.CreatedAt, UpdatedAt: profile.User.UpdatedAt,
		},
		Account: accountResponse{
			ID: profile.Account.ID, Currency: profile.Account.Currency, Balance: profile.Account.Balance,
			CreatedAt: profile.Account.CreatedAt, UpdatedAt: profile.Account.UpdatedAt,
		},
	}
}
