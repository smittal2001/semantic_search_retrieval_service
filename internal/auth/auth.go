package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/smittal2001/semantic-search/internal/models"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Claims is the payload embedded in each JWT.
type Claims struct {
	TenantID string `json:"tenant_id"`
	jwt.RegisteredClaims
}

// Validator parses and verifies JWTs.
type Validator struct {
	secret []byte
}

// NewValidator creates a Validator using the given HMAC secret.
func NewValidator(secret string) *Validator {
	return &Validator{secret: []byte(secret)}
}

// ParseToken verifies a JWT and returns its claims.
func (v *Validator) ParseToken(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return v.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}
	if claims.TenantID == "" {
		return nil, fmt.Errorf("token missing tenant_id claim")
	}
	return claims, nil
}

// UnaryInterceptor returns a gRPC unary server interceptor that:
//  1. Extracts the Bearer token from gRPC metadata
//  2. Validates it
//  3. Injects the tenant_id into the request context
//
// All downstream handlers read tenant_id from context
func (v *Validator) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Health check endpoint bypasses auth.
		if strings.HasSuffix(info.FullMethod, "/Healthz") {
			return handler(ctx, req)
		}

		tenantID, err := v.extractTenant(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "auth: %v", err)
		}

		ctx = context.WithValue(ctx, models.ContextKeyTenantID, tenantID)
		return handler(ctx, req)
	}
}

// TenantFromCtx extracts the tenant ID injected by the auth interceptor.
// Panics if the interceptor was not applied implying a programming error,
// not a runtime condition.
func TenantFromCtx(ctx context.Context) string {
	v, ok := ctx.Value(models.ContextKeyTenantID).(string)
	if !ok || v == "" {
		panic("tenant_id missing from context — auth interceptor not applied")
	}
	return v
}

func (v *Validator) extractTenant(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", fmt.Errorf("missing metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return "", fmt.Errorf("missing authorization header")
	}
	parts := strings.SplitN(vals[0], " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", fmt.Errorf("authorization header must be 'Bearer <token>'")
	}
	claims, err := v.ParseToken(parts[1])
	if err != nil {
		return "", err
	}
	return claims.TenantID, nil
}
