package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CleanSign/cleansign/server/internal/domain/auth"
	"github.com/CleanSign/cleansign/server/internal/domain/user"
	"github.com/CleanSign/cleansign/server/internal/shared/clock"
	"github.com/CleanSign/cleansign/server/internal/shared/errs"
)

type fakeStore struct {
	mu       sync.Mutex
	codes    map[string]auth.EmailCode
	users    map[string]user.User // email → user
	sessions map[string]auth.Session
}

func newStore() *fakeStore {
	return &fakeStore{codes: map[string]auth.EmailCode{}, users: map[string]user.User{}, sessions: map[string]auth.Session{}}
}

// CodeRepository
func (f *fakeStore) Get(_ context.Context, email string) (auth.EmailCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.codes[email]
	if !ok {
		return auth.EmailCode{}, errs.ErrNotFound
	}
	return c, nil
}
func (f *fakeStore) Upsert(_ context.Context, c auth.EmailCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c.Attempts = 0
	f.codes[c.Email] = c
	return nil
}
func (f *fakeStore) IncrementAttempts(_ context.Context, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.codes[email]
	c.Attempts++
	f.codes[email] = c
	return nil
}
func (f *fakeStore) Delete(_ context.Context, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.codes, email)
	return nil
}
func (f *fakeStore) DeleteExpired(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, c := range f.codes {
		if c.ExpiresAt.Before(now) {
			delete(f.codes, k)
			n++
		}
	}
	return n, nil
}

// codeRepo/userStore/sessionRepo are thin wrappers exposing the same fakeStore as different ports.
// (Names like Get and Delete collide, so one type cannot implement three ports. The code port uses fakeStore's methods directly)
type codeRepo struct{ *fakeStore }

type userStore struct{ *fakeStore }

func (u userStore) GetByEmail(_ context.Context, email string) (user.User, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x, ok := u.users[email]
	if !ok {
		return user.User{}, errs.ErrNotFound
	}
	return x, nil
}
func (u userStore) Create(_ context.Context, x user.User) (user.User, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.users[x.Email] = x
	return x, nil
}

type sessionRepo struct{ *fakeStore }

func (s sessionRepo) Create(_ context.Context, x auth.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[x.ID] = x
	return nil
}
func (s sessionRepo) Get(_ context.Context, id string) (auth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.sessions[id]
	if !ok {
		return auth.Session{}, errs.ErrNotFound
	}
	return x, nil
}
func (s sessionRepo) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}
func (s sessionRepo) DeleteByUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, x := range s.sessions {
		if x.UserID == userID {
			delete(s.sessions, k)
		}
	}
	return nil
}
func (s sessionRepo) DeleteExpired(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for k, x := range s.sessions {
		if x.ExpiresAt.Before(now) {
			delete(s.sessions, k)
			n++
		}
	}
	return n, nil
}

// fakeMailer remembers the code it sent.
type fakeMailer struct {
	last string
	to   string
	fail error
}

func (m *fakeMailer) SendCode(_ context.Context, to, code string, _ time.Duration) error {
	if m.fail != nil {
		return m.fail
	}
	m.to, m.last = to, code
	return nil
}

// fakeTokens creates fake tokens of the form "kind|user|session|exp".
type fakeTokens struct{ now func() time.Time }

func (f fakeTokens) Access(userID, sessionID string, now time.Time) (string, time.Time, error) {
	exp := now.Add(auth.AccessTTL)
	return fmt.Sprintf("access|%s|%s|%d", userID, sessionID, exp.Unix()), exp, nil
}
func (f fakeTokens) Refresh(userID, sessionID string, exp time.Time) (string, error) {
	return fmt.Sprintf("refresh|%s|%s|%d", userID, sessionID, exp.Unix()), nil
}
func (f fakeTokens) Parse(token string, ignoreExpiry bool) (Claims, error) {
	p := strings.Split(token, "|")
	if len(p) != 4 {
		return Claims{}, errors.New("bad token")
	}
	var exp int64
	fmt.Sscan(p[3], &exp)
	if !ignoreExpiry && !f.now().Before(time.Unix(exp, 0)) {
		return Claims{}, errors.New("expired")
	}
	return Claims{Kind: TokenKind(p[0]), UserID: p[1], SessionID: p[2]}, nil
}

type fixture struct {
	svc   Service
	store *fakeStore
	mail  *fakeMailer
	clk   *clock.Fixed
}

func newFixture() *fixture {
	store := newStore()
	mail := &fakeMailer{}
	clk := &clock.Fixed{T: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	svc := NewService(codeRepo{store}, userStore{store}, sessionRepo{store}, mail, fakeTokens{now: clk.Now}, clk, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &fixture{svc: svc, store: store, mail: mail, clk: clk}
}

func TestSendCode(t *testing.T) {
	f := newFixture()
	ctx := context.Background()

	if _, err := f.svc.SendCode(ctx, "not-an-email"); !errors.Is(err, user.ErrInvalidEmail) {
		t.Errorf("이메일 형식 오류: %v", err)
	}
	wait, err := f.svc.SendCode(ctx, "  User@Example.com ")
	if err != nil || wait != auth.ResendCooldown {
		t.Fatalf("SendCode: %v %v", wait, err)
	}
	if f.mail.to != "user@example.com" || len(f.mail.last) != 6 {
		t.Errorf("정규화한 주소로 6자리 코드를 보내야 한다: %q %q", f.mail.to, f.mail.last)
	}
	stored := f.store.codes["user@example.com"]
	if stored.CodeHash == "" || strings.Contains(stored.CodeHash, f.mail.last) {
		t.Error("코드는 해시로만 저장한다")
	}

	// cannot resend within the cooldown
	f.clk.Advance(10 * time.Second)
	var retry *auth.RetryAfterError
	if _, err := f.svc.SendCode(ctx, "user@example.com"); !errors.As(err, &retry) || retry.After != 50*time.Second {
		t.Errorf("쿨다운 오류: %v", err)
	}
	// after the cooldown, a new code is sent and the old code becomes invalid
	old := f.mail.last
	f.clk.Advance(51 * time.Second)
	if _, err := f.svc.SendCode(ctx, "user@example.com"); err != nil {
		t.Fatalf("재발송: %v", err)
	}
	if old != f.mail.last { // skip if the same code happened to come up
		if _, err := f.svc.Confirm(ctx, "user@example.com", old, false); !errors.Is(err, auth.ErrCodeInvalid) {
			t.Errorf("이전 코드는 거절돼야 한다: %v", err)
		}
	}
}

func TestSendCodeWindowLimit(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	step := auth.ResendCooldown + time.Second
	for i := 0; i < auth.MaxSendsPerWindow; i++ {
		if _, err := f.svc.SendCode(ctx, "a@b.com"); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
		f.clk.Advance(step)
	}
	var retry *auth.RetryAfterError
	if _, err := f.svc.SendCode(ctx, "a@b.com"); !errors.As(err, &retry) {
		t.Fatalf("6th send in the window must be refused: %v", err)
	}
	if want := auth.SendWindow - time.Duration(auth.MaxSendsPerWindow)*step; retry.After != want {
		t.Errorf("wait until the window ends: got %v want %v", retry.After, want)
	}
	f.clk.Advance(retry.After)
	if _, err := f.svc.SendCode(ctx, "a@b.com"); err != nil {
		t.Errorf("a new window starts after an hour: %v", err)
	}
	if f.store.codes["a@b.com"].SendCount != 1 {
		t.Errorf("counter restarts at 1: %+v", f.store.codes["a@b.com"])
	}
}

func TestSendCodeMailFailureAllowsRetry(t *testing.T) {
	f := newFixture()
	f.mail.fail = errors.New("smtp down")
	if _, err := f.svc.SendCode(context.Background(), "a@b.com"); !errors.Is(err, auth.ErrMailSend) {
		t.Fatalf("mail failure must be ErrMailSend: %v", err)
	}
	f.mail.fail = nil
	if _, err := f.svc.SendCode(context.Background(), "a@b.com"); err != nil {
		t.Errorf("보내지 못했으면 쿨다운 없이 다시 보낼 수 있어야 한다: %v", err)
	}
}

func TestConfirmSignsUpAndLogsIn(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	f.svc.SendCode(ctx, "new@user.com")

	res, err := f.svc.Confirm(ctx, "NEW@user.com", f.mail.last, false)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if res.User.Email != "new@user.com" || !res.User.EmailVerified || res.User.ID == "" {
		t.Errorf("신규 가입: %+v", res.User)
	}
	if _, ok := f.store.codes["new@user.com"]; ok {
		t.Error("성공하면 코드를 지운다")
	}
	if len(f.store.sessions) != 1 {
		t.Errorf("세션이 하나 생겨야 한다: %d", len(f.store.sessions))
	}
	if want := f.clk.Now().Add(auth.RefreshTTL); !res.Tokens.RefreshExpiresAt.Equal(want) {
		t.Errorf("refresh 수명 72시간: %v", res.Tokens.RefreshExpiresAt)
	}

	// logging in again with the same email gives the same user
	f.clk.Advance(2 * time.Minute)
	f.svc.SendCode(ctx, "new@user.com")
	again, err := f.svc.Confirm(ctx, "new@user.com", f.mail.last, true)
	if err != nil || again.User.ID != res.User.ID {
		t.Errorf("기존 사용자로 로그인: %+v %v", again.User, err)
	}
	if want := f.clk.Now().Add(auth.RememberRefreshTTL); !again.Tokens.RefreshExpiresAt.Equal(want) {
		t.Errorf("기기 기억이면 30일: %v", again.Tokens.RefreshExpiresAt)
	}
}

func TestConfirmRejections(t *testing.T) {
	f := newFixture()
	ctx := context.Background()

	if _, err := f.svc.Confirm(ctx, "a@b.com", "123456", false); !errors.Is(err, auth.ErrCodeExpired) {
		t.Errorf("코드를 요청한 적이 없으면 만료로 본다: %v", err)
	}

	f.svc.SendCode(ctx, "a@b.com")
	good := f.mail.last
	wrong := "000000"
	if wrong == good {
		wrong = "111111"
	}
	for i := 0; i < auth.MaxAttempts; i++ {
		if _, err := f.svc.Confirm(ctx, "a@b.com", wrong, false); !errors.Is(err, auth.ErrCodeInvalid) {
			t.Fatalf("틀린 코드 %d번째: %v", i+1, err)
		}
	}
	if _, err := f.svc.Confirm(ctx, "a@b.com", good, false); !errors.Is(err, auth.ErrTooManyAttempts) {
		t.Errorf("시도 초과면 맞는 코드도 거절: %v", err)
	}

	// expiry
	f.clk.Advance(auth.ResendCooldown + time.Second)
	f.svc.SendCode(ctx, "a@b.com")
	f.clk.Advance(auth.CodeTTL)
	if _, err := f.svc.Confirm(ctx, "a@b.com", f.mail.last, false); !errors.Is(err, auth.ErrCodeExpired) {
		t.Errorf("만료된 코드: %v", err)
	}
	if len(f.store.sessions) != 0 {
		t.Error("실패하면 세션이 생기면 안 된다")
	}
}

func login(t *testing.T, f *fixture) ConfirmResult {
	t.Helper()
	f.svc.SendCode(context.Background(), "u@x.com")
	res, err := f.svc.Confirm(context.Background(), "u@x.com", f.mail.last, false)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAuthenticateRefreshLogout(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	res := login(t, f)

	uid, _, err := f.svc.Authenticate(ctx, res.Tokens.Access)
	if err != nil || uid != res.User.ID {
		t.Fatalf("Authenticate: %v %v", uid, err)
	}
	if _, _, err := f.svc.Authenticate(ctx, res.Tokens.Refresh); !errors.Is(err, errs.ErrUnauthorized) {
		t.Errorf("refresh 토큰으로 인증할 수 없다: %v", err)
	}
	if _, err := f.svc.Refresh(ctx, res.Tokens.Access); !errors.Is(err, errs.ErrUnauthorized) {
		t.Errorf("access 토큰으로 갱신할 수 없다: %v", err)
	}

	// even after access expires, refresh yields a new access. The refresh token stays the same
	f.clk.Advance(auth.AccessTTL + time.Minute)
	if _, _, err := f.svc.Authenticate(ctx, res.Tokens.Access); !errors.Is(err, errs.ErrUnauthorized) {
		t.Errorf("만료된 access: %v", err)
	}
	renewed, err := f.svc.Refresh(ctx, res.Tokens.Refresh)
	if err != nil || renewed.Refresh != res.Tokens.Refresh {
		t.Fatalf("Refresh: %+v %v", renewed, err)
	}
	if _, _, err := f.svc.Authenticate(ctx, renewed.Access); err != nil {
		t.Errorf("새 access는 유효해야 한다: %v", err)
	}

	// after logout neither token can be used. Logout also works with an expired token
	f.clk.Advance(auth.AccessTTL + time.Minute)
	if err := f.svc.Logout(ctx, renewed.Access); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Authenticate(ctx, renewed.Access); !errors.Is(err, errs.ErrUnauthorized) {
		t.Errorf("로그아웃 뒤 access: %v", err)
	}
	if _, err := f.svc.Refresh(ctx, res.Tokens.Refresh); !errors.Is(err, errs.ErrUnauthorized) {
		t.Errorf("로그아웃 뒤 refresh: %v", err)
	}
	if err := f.svc.Logout(ctx, ""); err != nil {
		t.Errorf("토큰이 없어도 로그아웃은 오류가 아니다: %v", err)
	}
	if err := f.svc.Logout(ctx, "garbage"); err != nil {
		t.Errorf("잘못된 토큰도 오류가 아니다: %v", err)
	}
}

func TestRefreshRejectsExpiredSession(t *testing.T) {
	f := newFixture()
	res := login(t, f)
	f.clk.Advance(auth.RefreshTTL + time.Minute)
	if _, err := f.svc.Refresh(context.Background(), res.Tokens.Refresh); !errors.Is(err, errs.ErrUnauthorized) {
		t.Errorf("만료된 refresh: %v", err)
	}
}

func TestRevokeAllAndPurge(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	res := login(t, f)

	if err := f.svc.RevokeAll(ctx, res.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Authenticate(ctx, res.Tokens.Access); !errors.Is(err, errs.ErrUnauthorized) {
		t.Errorf("전체 폐기 뒤: %v", err)
	}

	f.store.codes["old@x.com"] = auth.EmailCode{Email: "old@x.com", ExpiresAt: f.clk.Now().Add(-time.Hour)}
	f.store.sessions["s"] = auth.Session{ID: "s", ExpiresAt: f.clk.Now().Add(-time.Hour)}
	if err := f.svc.PurgeExpired(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.store.codes) != 0 || len(f.store.sessions) != 0 {
		t.Errorf("만료된 것은 지워야 한다: %d %d", len(f.store.codes), len(f.store.sessions))
	}
}
