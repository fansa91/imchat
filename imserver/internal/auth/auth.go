package auth

import (
	"errors"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type Manager struct {
	secret []byte
	ttl    time.Duration
}

func New(secret string, ttl time.Duration) *Manager {
	return &Manager{secret: []byte(secret), ttl: ttl}
}

func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

func (m *Manager) Issue(uid uint64) (string, time.Time, error) {
	exp := time.Now().Add(m.ttl)
	claims := jwt.RegisteredClaims{
		Subject:   strconv.FormatUint(uid, 10),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(exp),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	return tok, exp, err
}

func (m *Manager) Verify(token string) (uint64, error) {
	claims := &jwt.RegisteredClaims{}
	t, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return m.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !t.Valid {
		return 0, errors.New("invalid token")
	}
	return strconv.ParseUint(claims.Subject, 10, 64)
}
