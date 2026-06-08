// Package auth handles password hashing and HMAC-signed tokens.
// Tokens are signed with HMAC-SHA256 and carry a user ID, username, and expiry.
// Format: base64url(json_payload).hex(hmac_signature)
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const tokenExpiry = 7 * 24 * time.Hour

// Claims is the signed payload inside a token.
type Claims struct {
	UserID   uuid.UUID `json:"uid"`
	Username string    `json:"usr"`
	Expiry   int64     `json:"exp"`
}

// HashPassword returns a bcrypt hash of the password.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword returns nil if password matches the stored hash.
func CheckPassword(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// CreateToken creates a signed token for the given user.
func CreateToken(secret []byte, userID uuid.UUID, username string) (string, error) {
	claims := Claims{
		UserID:   userID,
		Username: username,
		Expiry:   time.Now().Add(tokenExpiry).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	sig := sign(secret, encoded)
	return encoded + "." + sig, nil
}

// ValidateToken parses and verifies a token, returning its claims.
func ValidateToken(secret []byte, token string) (*Claims, error) {
	dot := strings.LastIndex(token, ".")
	if dot < 0 {
		return nil, fmt.Errorf("invalid token format")
	}
	encoded, sig := token[:dot], token[dot+1:]
	if !hmac.Equal([]byte(sign(secret, encoded)), []byte(sig)) {
		return nil, fmt.Errorf("invalid token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid token encoding")
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("invalid token payload")
	}
	if time.Now().Unix() > claims.Expiry {
		return nil, fmt.Errorf("token expired")
	}
	return &claims, nil
}

func sign(secret []byte, data string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}
