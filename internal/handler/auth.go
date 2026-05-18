package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/example/booking-engine/internal/service"
)

type ctxKey int

const actorKey ctxKey = 1

func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, err := h.extractUserID(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error(), nil)
			return
		}
		u, err := h.users.Get(r.Context(), userID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unknown user in token", nil)
			return
		}
		ctx := context.WithValue(r.Context(), actorKey, service.ActorFromUser(u))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *Handler) extractUserID(r *http.Request) (string, error) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		return "", errors.New("Authorization: Bearer token required")
	}
	raw := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
	if raw == "" {
		return "", errors.New("empty bearer token")
	}

	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return h.jwtSecret, nil
	})
	if err != nil || !tok.Valid {
		return "", errors.New("invalid token")
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return "", errors.New("invalid claims")
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", errors.New("token missing sub claim")
	}
	return sub, nil
}

func actorFromCtx(ctx context.Context) service.Actor {
	a, _ := ctx.Value(actorKey).(service.Actor)
	return a
}
