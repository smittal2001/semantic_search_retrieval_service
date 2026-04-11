// gen-jwt generates a short-lived JWT for local development and testing.
// It signs with the same JWT_SECRET used by the services.
//
// Usage:
//
//	JWT_SECRET=dev-secret-change-in-prod go run ./scripts/gen-jwt
//	JWT_SECRET=dev-secret-change-in-prod go run ./scripts/gen-jwt --tenant-id=my-tenant
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yourname/semantic-search/internal/auth"
)

func main() {
	tenantID := flag.String("tenant-id", "dev-tenant", "tenant ID to embed in the token")
	ttl      := flag.Duration("ttl", 24*time.Hour, "token lifetime")
	flag.Parse()

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "dev-secret-change-in-prod"
	}

	claims := auth.Claims{
		TenantID: *tenantID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(*ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   *tenantID,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign token: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(signed)
}
