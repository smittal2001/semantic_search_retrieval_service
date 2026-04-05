package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yourname/semantic-search/internal/auth"
	"github.com/yourname/semantic-search/internal/models"
	"google.golang.org/grpc/metadata"
)

const testSecret = "test-secret-key-min-32-chars-long!!"

func makeToken(t *testing.T, tenantID string, expiresIn time.Duration) string {
	t.Helper()
	claims := auth.Claims{
		TenantID: tenantID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiresIn)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// TestParseToken_Valid verifies a well-formed token returns correct claims.
func TestParseToken_Valid(t *testing.T) {
	v := auth.NewValidator(testSecret)
	token := makeToken(t, "tenant-abc", time.Hour)

	claims, err := v.ParseToken(token)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if claims.TenantID != "tenant-abc" {
		t.Errorf("expected tenant-abc, got %q", claims.TenantID)
	}
}

// TestParseToken_Expired verifies expired tokens are rejected.
func TestParseToken_Expired(t *testing.T) {
	v := auth.NewValidator(testSecret)
	token := makeToken(t, "tenant-abc", -time.Minute) // expired 1 minute ago

	_, err := v.ParseToken(token)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

// TestParseToken_WrongSecret verifies tokens signed with a different secret fail.
func TestParseToken_WrongSecret(t *testing.T) {
	v := auth.NewValidator("different-secret-key-min-32-chars!!")
	token := makeToken(t, "tenant-abc", time.Hour)

	_, err := v.ParseToken(token)
	if err == nil {
		t.Fatal("expected error for wrong secret, got nil")
	}
}

// TestParseToken_MissingTenantID verifies tokens without tenant_id are rejected.
func TestParseToken_MissingTenantID(t *testing.T) {
	// Build a token with an empty tenant ID
	claims := auth.Claims{
		TenantID: "", // missing
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, _ := token.SignedString([]byte(testSecret))

	v := auth.NewValidator(testSecret)
	_, err := v.ParseToken(signed)
	if err == nil {
		t.Fatal("expected error for missing tenant_id, got nil")
	}
}

// TestTenantFromCtx_Present verifies extraction succeeds when tenant is in context.
func TestTenantFromCtx_Present(t *testing.T) {
	ctx := context.WithValue(context.Background(), models.ContextKeyTenantID, "tenant-xyz")
	got := auth.TenantFromCtx(ctx)
	if got != "tenant-xyz" {
		t.Errorf("expected tenant-xyz, got %q", got)
	}
}

// TestTenantFromCtx_Missing verifies extraction panics when tenant is absent.
// This is a programming error (middleware not applied) — panic is correct.
func TestTenantFromCtx_Missing(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for missing tenant_id, got none")
		}
	}()
	auth.TenantFromCtx(context.Background()) // should panic
}

// TestUnaryInterceptor_InjectsTenantID verifies the gRPC interceptor injects
// the tenant_id from the JWT into the context for the downstream handler.
func TestUnaryInterceptor_InjectsTenantID(t *testing.T) {
	v := auth.NewValidator(testSecret)
	interceptor := v.UnaryInterceptor()

	token := makeToken(t, "tenant-injected", time.Hour)

	// Build an incoming gRPC context with the Authorization metadata
	md := metadata.Pairs("authorization", "Bearer "+token)
	ctx := metadata.NewIncomingContext(context.Background(), md)

	var capturedTenantID string
	_, err := interceptor(ctx, nil, nil, func(ctx context.Context, req interface{}) (interface{}, error) {
		capturedTenantID = auth.TenantFromCtx(ctx)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("interceptor error: %v", err)
	}
	if capturedTenantID != "tenant-injected" {
		t.Errorf("expected tenant-injected, got %q", capturedTenantID)
	}
}

// TestUnaryInterceptor_MissingHeader verifies unauthenticated requests are rejected.
func TestUnaryInterceptor_MissingHeader(t *testing.T) {
	v := auth.NewValidator(testSecret)
	interceptor := v.UnaryInterceptor()

	// Context with no authorization metadata
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})

	_, err := interceptor(ctx, nil, nil, func(ctx context.Context, req interface{}) (interface{}, error) {
		t.Error("handler should not be called for unauthenticated request")
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected unauthenticated error, got nil")
	}
}
