// Package auth assembles the email verification-code login and session flow.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/CleanSign/cleansign/server/internal/domain/auth"
	"github.com/CleanSign/cleansign/server/internal/domain/user"
	"github.com/CleanSign/cleansign/server/internal/shared/clock"
	"github.com/CleanSign/cleansign/server/internal/shared/errs"
	"github.com/CleanSign/cleansign/server/internal/shared/ids"
)

// 생성 패키지 이름을 임시로 다르게 주는 이유: domain/auth와 이름이 같으면 생성기가 `auth.` 접두사를 지워 버린다.
//
//go:generate go tool ifacemaker -f service.go -s service -i Service -p authgen -o service_interface.go -y "Service는 auth 흐름의 공개 메서드다. handler가 이 인터페이스에 의존하고, 테스트에서는 mock으로 바꾼다."
//go:generate perl -pi -e "s/^package authgen/package auth/; s/auth[.]ConfirmResult/ConfirmResult/g" service_interface.go
type service struct {
	codes      CodeRepository
	users      UserStore
	sessions   SessionRepository
	mailer     Mailer
	tokens     TokenIssuer
	clock      clock.Clock
	codeSecret string
	log        *slog.Logger
}

func NewService(codes CodeRepository, users UserStore, sessions SessionRepository, mailer Mailer, tokens TokenIssuer, clk clock.Clock, codeSecret string, log *slog.Logger) Service {
	return &service{codes: codes, users: users, sessions: sessions, mailer: mailer, tokens: tokens, clock: clk, codeSecret: codeSecret, log: log}
}

// SendCode sends a 6-digit verification code by email. Resending requires the cooldown to have passed.
// The returned value is how long to wait before the next request.
func (s *service) SendCode(ctx context.Context, rawEmail string) (time.Duration, error) {
	email, err := user.NormalizeEmail(rawEmail)
	if err != nil {
		return 0, err
	}
	now := s.clock.Now()

	existing, err := s.codes.Get(ctx, email)
	switch {
	case err == nil:
		if wait, ok := existing.CanResend(now); !ok {
			return 0, &auth.RetryAfterError{After: wait}
		}
	case !errors.Is(err, errs.ErrNotFound):
		return 0, err
	}

	code, err := auth.NewCode()
	if err != nil {
		return 0, err
	}
	sendCount, windowStartedAt := existing.NextSend(now)
	if err := s.codes.Upsert(ctx, auth.EmailCode{
		Email:           email,
		CodeHash:        auth.HashCode(s.codeSecret, email, code),
		SentAt:          now,
		ExpiresAt:       now.Add(auth.CodeTTL),
		SendCount:       sendCount,
		WindowStartedAt: windowStartedAt,
	}); err != nil {
		return 0, err
	}
	if err := s.mailer.SendCode(ctx, email, code, auth.CodeTTL); err != nil {
		// nothing was sent: drop the row so the user can retry right away, and don't count it against the window
		if delErr := s.codes.Delete(ctx, email); delErr != nil {
			s.log.Warn("failed to delete unsent code", "error", delErr)
		}
		return 0, fmt.Errorf("%w: %w", auth.ErrMailSend, err)
	}
	return auth.ResendCooldown, nil
}

// ConfirmResult is the result of a successful login.
type ConfirmResult struct {
	User   user.User
	Tokens auth.Tokens
}

// Confirm verifies the code and logs in. An unseen email is signed up.
func (s *service) Confirm(ctx context.Context, rawEmail, code string, rememberDevice bool) (ConfirmResult, error) {
	email, err := user.NormalizeEmail(rawEmail)
	if err != nil {
		return ConfirmResult{}, err
	}
	now := s.clock.Now()

	stored, err := s.codes.Get(ctx, email)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return ConfirmResult{}, auth.ErrCodeExpired
		}
		return ConfirmResult{}, err
	}
	if err := stored.Check(now, auth.HashCode(s.codeSecret, email, code)); err != nil {
		switch {
		case errors.Is(err, auth.ErrCodeInvalid):
			if incErr := s.codes.IncrementAttempts(ctx, email); incErr != nil {
				s.log.Warn("failed to count attempt", "error", incErr)
			}
		case errors.Is(err, auth.ErrCodeExpired):
			if delErr := s.codes.Delete(ctx, email); delErr != nil {
				s.log.Warn("failed to delete expired code", "error", delErr)
			}
		}
		return ConfirmResult{}, err
	}
	if err := s.codes.Delete(ctx, email); err != nil {
		return ConfirmResult{}, err
	}

	u, err := s.loginOrCreate(ctx, email, now)
	if err != nil {
		return ConfirmResult{}, err
	}

	session := auth.Session{
		ID:             ids.New(),
		UserID:         u.ID,
		RememberDevice: rememberDevice,
		ExpiresAt:      now.Add(auth.RefreshTTLFor(rememberDevice)),
		CreatedAt:      now,
	}
	if err := s.sessions.Create(ctx, session); err != nil {
		return ConfirmResult{}, err
	}
	tokens, err := s.issue(u.ID, session, now)
	if err != nil {
		return ConfirmResult{}, err
	}
	return ConfirmResult{User: u, Tokens: tokens}, nil
}

func (s *service) loginOrCreate(ctx context.Context, email string, now time.Time) (user.User, error) {
	u, err := s.users.GetByEmail(ctx, email)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, errs.ErrNotFound) {
		return user.User{}, err
	}
	created, err := s.users.Create(ctx, user.User{
		ID:            ids.New(),
		Email:         email,
		EmailVerified: true, // the email was verified by the code
		CreatedAt:     now,
	})
	if err != nil {
		// concurrent signup with the same email: use the user created first
		if again, getErr := s.users.GetByEmail(ctx, email); getErr == nil {
			return again, nil
		}
		return user.User{}, err
	}
	return created, nil
}

func (s *service) issue(userID string, session auth.Session, now time.Time) (auth.Tokens, error) {
	access, accessExp, err := s.tokens.Access(userID, session.ID, now)
	if err != nil {
		return auth.Tokens{}, err
	}
	refresh, err := s.tokens.Refresh(userID, session.ID, session.ExpiresAt)
	if err != nil {
		return auth.Tokens{}, err
	}
	return auth.Tokens{Access: access, Refresh: refresh, AccessExpiresAt: accessExp, RefreshExpiresAt: session.ExpiresAt}, nil
}

// Refresh issues a new access token from the refresh token. The refresh token itself does not change.
func (s *service) Refresh(ctx context.Context, refreshToken string) (auth.Tokens, error) {
	claims, err := s.tokens.Parse(refreshToken, false)
	if err != nil || claims.Kind != KindRefresh {
		return auth.Tokens{}, errs.ErrUnauthorized
	}
	session, err := s.activeSession(ctx, claims)
	if err != nil {
		return auth.Tokens{}, err
	}
	access, accessExp, err := s.tokens.Access(claims.UserID, session.ID, s.clock.Now())
	if err != nil {
		return auth.Tokens{}, err
	}
	return auth.Tokens{Access: access, Refresh: refreshToken, AccessExpiresAt: accessExp, RefreshExpiresAt: session.ExpiresAt}, nil
}

// Authenticate verifies the user and session the access token points to. Logged-out sessions are rejected.
func (s *service) Authenticate(ctx context.Context, accessToken string) (userID, sessionID string, err error) {
	claims, err := s.tokens.Parse(accessToken, false)
	if err != nil || claims.Kind != KindAccess {
		return "", "", errs.ErrUnauthorized
	}
	session, err := s.activeSession(ctx, claims)
	if err != nil {
		return "", "", err
	}
	return claims.UserID, session.ID, nil
}

func (s *service) activeSession(ctx context.Context, claims Claims) (auth.Session, error) {
	session, err := s.sessions.Get(ctx, claims.SessionID)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return auth.Session{}, errs.ErrUnauthorized
		}
		return auth.Session{}, err
	}
	if session.UserID != claims.UserID || !session.IsActive(s.clock.Now()) {
		return auth.Session{}, errs.ErrUnauthorized
	}
	return session, nil
}

// Logout revokes the session the token points to. Even an expired token revokes it if the signature is valid.
// It is not an error if it was already revoked or there is no token.
func (s *service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	claims, err := s.tokens.Parse(token, true)
	if err != nil {
		return nil
	}
	return s.sessions.Delete(ctx, claims.SessionID)
}

// RevokeAll revokes all of the user's sessions.
func (s *service) RevokeAll(ctx context.Context, userID string) error {
	return s.sessions.DeleteByUser(ctx, userID)
}

// PurgeExpired deletes expired verification codes and sessions.
func (s *service) PurgeExpired(ctx context.Context) error {
	now := s.clock.Now()
	codes, err := s.codes.DeleteExpired(ctx, now)
	if err != nil {
		return err
	}
	sessions, err := s.sessions.DeleteExpired(ctx, now)
	if err != nil {
		return err
	}
	if codes+sessions > 0 {
		s.log.Info("purged expired auth data", "codes", codes, "sessions", sessions)
	}
	return nil
}
