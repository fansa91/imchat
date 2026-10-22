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

// Claims 登录 token。TV 是签发时用户的 token_version（v0.4.0）：
// 改密/重置后服务端递增 users.token_version，旧 token 的 tv 对不上即失效。
// v0.4.0 之前签发的 token 没有 tv claim，按 0 处理（用户从未改密时版本也是 0，继续有效）。
type Claims struct {
	jwt.RegisteredClaims
	TV int `json:"tv"`
}

func (m *Manager) Issue(uid uint64, tv int) (string, time.Time, error) {
	exp := time.Now().Add(m.ttl)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatUint(uid, 10),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		TV: tv,
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	return tok, exp, err
}

// Verify 校验签名与过期，返回用户 ID 与 token 中的 tv（调用方再与当前 token_version 比对）。
func (m *Manager) Verify(token string) (uint64, int, error) {
	claims := &Claims{}
	t, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return m.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !t.Valid {
		return 0, 0, errors.New("invalid token")
	}
	uid, err := strconv.ParseUint(claims.Subject, 10, 64)
	if err != nil {
		return 0, 0, errors.New("invalid token")
	}
	return uid, claims.TV, nil
}
