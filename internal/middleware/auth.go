package middleware

import (
	"context"
	"crypto/rsa"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"go_warehouse_service/internal/auth"
)

type contextKey string

const UserContextKey contextKey = "user"

// UserClaims описывает данные пользователя из JWT-токена Symfony.
//
// ⚠ Roles здесь — СЫРЫЕ роли из User::getRoles(), без иерархии:
// у администратора портала будет ровно ["ROLE_ADMIN"]. Разворачивает иерархию
// пакет internal/auth — использовать надо его функции, а не сравнивать строки.
type UserClaims struct {
	ID       int64    `json:"id"`
	Username string   `json:"username"`
	Roles    []string `json:"roles"`
}

// Has сообщает, есть ли роль с учётом иерархии Symfony.
func (c UserClaims) Has(role string) bool { return auth.Has(c.Roles, role) }

// IsInventoryAdmin — администратор инвентаризации (в т.ч. через ROLE_ADMIN).
func (c UserClaims) IsInventoryAdmin() bool { return auth.IsInventoryAdmin(c.Roles) }

// CanWrite — право менять данные инвентаризации.
func (c UserClaims) CanWrite() bool { return auth.CanWrite(c.Roles) }

// CanRead — право видеть данные инвентаризации.
func (c UserClaims) CanRead() bool { return auth.CanRead(c.Roles) }

type AuthMiddleware struct {
	publicKey *rsa.PublicKey
}

// NewAuthMiddleware загружает публичный ключ RS256 и инициализирует middleware
func NewAuthMiddleware(publicKeyPath string) (*AuthMiddleware, error) {
	keyBytes, err := os.ReadFile(publicKeyPath)
	if err != nil {
		slog.Error("Ошибка чтения публичного ключа JWT", "path", publicKeyPath, "error", err)
		return nil, err
	}

	pubKey, err := jwt.ParseRSAPublicKeyFromPEM(keyBytes)
	if err != nil {
		slog.Error("Ошибка парсинга публичного ключа JWT", "error", err)
		return nil, err
	}

	return &AuthMiddleware{publicKey: pubKey}, nil
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized","code":"unauthorized"}`))
}

// Handler проверяет JWT-токен и кладёт Claims в контекст запроса
func (m *AuthMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			unauthorized(w)
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			unauthorized(w)
			return
		}

		// Критично: WithExpirationRequired() требует наличия и валидности exp.
		// Без этого просроченные/украденные токены работали бы вечно.
		token, err := jwt.Parse(parts[1], func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, errors.New("unexpected signing method")
			}
			return m.publicKey, nil
		}, jwt.WithExpirationRequired(), jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}))

		if err != nil || !token.Valid {
			unauthorized(w)
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			unauthorized(w)
			return
		}

		var userClaims UserClaims

		if idVal, exists := claims["id"]; exists {
			switch v := idVal.(type) {
			case float64:
				userClaims.ID = int64(v)
			case int64:
				userClaims.ID = v
			}
		}

		if usernameVal, exists := claims["username"]; exists {
			if usernameStr, ok := usernameVal.(string); ok {
				userClaims.Username = usernameStr
			}
		}

		if rolesVal, exists := claims["roles"]; exists {
			if rolesSlice, ok := rolesVal.([]interface{}); ok {
				for _, rVal := range rolesSlice {
					if rStr, ok := rVal.(string); ok {
						userClaims.Roles = append(userClaims.Roles, rStr)
					}
				}
			}
		}

		// Без id токен не годится микросервису: все журналы пишутся на пользователя.
		if userClaims.ID == 0 {
			unauthorized(w)
			return
		}

		ctx := context.WithValue(r.Context(), UserContextKey, userClaims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetUser достаёт информацию о пользователе из контекста запроса
func GetUser(ctx context.Context) (UserClaims, bool) {
	user, ok := ctx.Value(UserContextKey).(UserClaims)
	return user, ok
}
