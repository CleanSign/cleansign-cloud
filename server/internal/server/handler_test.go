package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/CleanSign/cleansign/server/internal/domain/auth"
	"github.com/CleanSign/cleansign/server/internal/domain/contract"
	"github.com/CleanSign/cleansign/server/internal/domain/user"
	authsvc "github.com/CleanSign/cleansign/server/internal/service/auth"
	"github.com/CleanSign/cleansign/server/internal/shared/errs"
)

// handler tests swap the services for mocks and check only "request -> response shape", with no DB, AI or mail.
// (integration tests live in internal/app/http_test.go)

type mocks struct {
	auth      *MockAuthService
	users     *MockUserService
	contracts *MockContractService
	handler   http.Handler
}

func newMocks(t *testing.T) *mocks {
	t.Helper()
	ctrl := gomock.NewController(t)
	m := &mocks{auth: NewMockAuthService(ctrl), users: NewMockUserService(ctrl), contracts: NewMockContractService(ctrl)}
	m.handler = New(Deps{Auth: m.auth, Users: m.users, Contracts: m.contracts, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return m
}

// loggedIn makes the "tok" token resolve to user u1.
func (m *mocks) loggedIn() {
	m.auth.EXPECT().Authenticate(gomock.Any(), "tok").Return("u1", "s1", nil).AnyTimes()
}

type reply struct {
	Status int
	Header http.Header
	Body   map[string]any
}

func (m *mocks) do(method, path string, body io.Reader, mods ...func(*http.Request)) reply {
	req := httptest.NewRequest(method, path, body)
	for _, f := range mods {
		f(req)
	}
	rec := httptest.NewRecorder()
	m.handler.ServeHTTP(rec, req)
	out := reply{Status: rec.Code, Header: rec.Header()}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.Body)
	return out
}

func bearer(r *http.Request) { r.Header.Set("Authorization", "Bearer tok") }

func jsonBody(r *http.Request) { r.Header.Set("Content-Type", "application/json") }

func TestContractsRequireLogin(t *testing.T) {
	m := newMocks(t) // no EXPECT on the services: calling one fails the test
	r := m.do("GET", "/api/v1/contracts/c1", nil)
	if r.Status != 401 || r.Body["error"] != "unauthorized" {
		t.Errorf("401 when not logged in: %d %v", r.Status, r.Body)
	}
}

func TestExpiredTokenIsTreatedAsLoggedOut(t *testing.T) {
	m := newMocks(t)
	m.auth.EXPECT().Authenticate(gomock.Any(), "old").Return("", "", errs.ErrUnauthorized)
	r := m.do("GET", "/api/v1/users/me", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer old") })
	if r.Status != 401 {
		t.Errorf("401 for an expired token: %d", r.Status)
	}
}

func TestGetContractNotFound(t *testing.T) {
	m := newMocks(t)
	m.loggedIn()
	m.contracts.EXPECT().Get(gomock.Any(), "u1", "nope").Return(contract.Contract{}, nil, errs.ErrNotFound)
	r := m.do("GET", "/api/v1/contracts/nope", nil, bearer)
	if r.Status != 404 || r.Body["error"] != "not found" {
		t.Errorf("404 when missing: %d %v", r.Status, r.Body)
	}
}

func TestGetContractPendingShape(t *testing.T) {
	m := newMocks(t)
	m.loggedIn()
	progress := contract.Progress{Stage: "classifying", Done: 1, Total: 3}
	m.contracts.EXPECT().Get(gomock.Any(), "u1", "c1").Return(contract.Contract{
		ID: "c1", FileName: "a.pdf", Status: contract.StatusPending, Progress: &progress, CreatedAt: time.Now(),
	}, nil, nil)

	r := m.do("GET", "/api/v1/contracts/c1", nil, bearer)
	if r.Status != 200 || r.Body["status"] != "pending" {
		t.Fatalf("pending is 200 too: %d %v", r.Status, r.Body)
	}
	if p, _ := r.Body["progress"].(map[string]any); p["stage"] != "classifying" || p["done"] != float64(1) {
		t.Errorf("progress: %v", r.Body["progress"])
	}
	for _, k := range []string{"riskScore", "grade", "lossRange", "depositAmount", "flow", "error"} {
		if v, ok := r.Body[k]; !ok || v != nil {
			t.Errorf("%s must be present and null: %v", k, v)
		}
	}
	if items, ok := r.Body["riskItems"].([]any); !ok || len(items) != 0 {
		t.Errorf("riskItems is an empty array: %v", r.Body["riskItems"])
	}
}

func TestListContractsPassesNormalizedPaging(t *testing.T) {
	m := newMocks(t)
	m.loggedIn()
	// page=abc is clamped to 1 and limit=1000 to 50 before reaching the service
	m.contracts.EXPECT().List(gomock.Any(), "u1", 1, 50).Return(contract.Page{Page: 1, Size: 50}, nil)
	if r := m.do("GET", "/api/v1/contracts?page=abc&limit=1000", nil, bearer); r.Status != 200 {
		t.Errorf("status: %d", r.Status)
	}
}

func TestHomeListUsesRecent(t *testing.T) {
	m := newMocks(t)
	m.loggedIn()
	m.contracts.EXPECT().Recent(gomock.Any(), "u1", 3).Return([]contract.Summary{{ID: "c1", Status: contract.StatusCompleted, ClauseCount: 2}}, nil)
	r := m.do("GET", "/api/v1/contracts?status=active&limit=3", nil, bearer)
	items, _ := r.Body["items"].([]any)
	if r.Status != 200 || len(items) != 1 || items[0].(map[string]any)["riskCount"] != float64(2) {
		t.Errorf("home list: %d %v", r.Status, r.Body)
	}
}

func upload(t *testing.T, filename, mime string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	h.Set("Content-Type", mime)
	part, _ := w.CreatePart(h)
	_, _ = part.Write(data)
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

func TestCreateContractErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		msg    string
	}{
		{"not a PDF", contract.ErrNotPDF, 415, "PDF 파일만 올릴 수 있어요"},
		{"too large", contract.ErrBadSize, 413, "파일 크기는 10MB 이하여야 해요"},
		{"unexpected error", io.ErrUnexpectedEOF, 500, "서버에 문제가 생겼어요"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMocks(t)
			m.loggedIn()
			m.contracts.EXPECT().Submit(gomock.Any(), "u1", "a.pdf", "application/pdf", gomock.Any(), gomock.Any()).Return("", c.err)
			body, ct := upload(t, "a.pdf", "application/pdf", []byte("%PDF"))
			r := m.do("POST", "/api/v1/contracts", body, bearer, func(r *http.Request) { r.Header.Set("Content-Type", ct) })
			if r.Status != c.status || r.Body["error"] != c.msg {
				t.Errorf("expected %d %q, got %d %v", c.status, c.msg, r.Status, r.Body)
			}
		})
	}
}

func TestCreateContractAccepted(t *testing.T) {
	m := newMocks(t)
	m.loggedIn()
	m.contracts.EXPECT().Submit(gomock.Any(), "u1", "lease.pdf", "application/pdf", int64(4), gomock.Any()).Return("c9", nil)
	body, ct := upload(t, "lease.pdf", "application/pdf", []byte("%PDF"))
	r := m.do("POST", "/api/v1/contracts", body, bearer, func(r *http.Request) { r.Header.Set("Content-Type", ct) })
	if r.Status != 202 || r.Body["id"] != "c9" || r.Body["status"] != "pending" {
		t.Errorf("202 + id: %d %v", r.Status, r.Body)
	}
}

func TestSendCodeRateLimited(t *testing.T) {
	m := newMocks(t)
	m.auth.EXPECT().SendCode(gomock.Any(), "a@b.com").Return(time.Duration(0), &auth.RetryAfterError{After: 42 * time.Second})
	r := m.do("POST", "/api/v1/auth/email/code", strings.NewReader(`{"email":"a@b.com"}`), jsonBody)
	if r.Status != 429 || r.Header.Get("Retry-After") != "42" {
		t.Errorf("429 with Retry-After: %d %v", r.Status, r.Header)
	}
}

func TestSendCodeBadJSON(t *testing.T) {
	m := newMocks(t)
	r := m.do("POST", "/api/v1/auth/email/code", strings.NewReader(`{bad`), jsonBody)
	if r.Status != 400 || r.Body["error"] != "요청 형식이 올바르지 않아요" {
		t.Errorf("malformed body: %d %v", r.Status, r.Body)
	}
}

func TestConsentValidationDoesNotReachService(t *testing.T) {
	m := newMocks(t)
	m.loggedIn() // no EXPECT on RecordConsent, so reaching the service fails the test
	r := m.do("POST", "/api/v1/users/me/consents", strings.NewReader(`{"terms":"yes","privacy":true}`), bearer, jsonBody)
	if r.Status != 400 || r.Body["error"] != "terms, privacy는 boolean이어야 합니다" {
		t.Errorf("type error message: %d %v", r.Status, r.Body)
	}
}

func TestConsentRequiredIsRejectedByValidationBeforeService(t *testing.T) {
	m := newMocks(t) // no RecordConsent expectation: false is rejected by the spec's validate rule
	m.loggedIn()
	for name, body := range map[string]string{
		"privacy refused": `{"terms":true,"privacy":false}`,
		"terms refused":   `{"terms":false,"privacy":true}`,
		"both refused":    `{"terms":false,"privacy":false}`,
	} {
		r := m.do("POST", "/api/v1/users/me/consents", strings.NewReader(body), bearer, jsonBody)
		if r.Status != 400 || r.Body["error"] != "이용약관과 개인정보처리방침 동의는 필수입니다" {
			t.Errorf("%s: %d %v", name, r.Status, r.Body)
		}
	}
}

func TestConsentAccepted(t *testing.T) {
	m := newMocks(t)
	m.loggedIn()
	m.users.EXPECT().RecordConsent(gomock.Any(), "u1", true, true, true).Return(user.Consent{ID: "k1", UserID: "u1", Terms: true, Privacy: true, Marketing: true}, nil)
	r := m.do("POST", "/api/v1/users/me/consents", strings.NewReader(`{"terms":true,"privacy":true,"marketing":true}`), bearer, jsonBody)
	if r.Status != 201 || r.Body["marketing"] != true {
		t.Errorf("201: %d %v", r.Status, r.Body)
	}
}

func TestLogoutClearsCookiesEvenForAnonymous(t *testing.T) {
	m := newMocks(t)
	m.auth.EXPECT().Logout(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	r := m.do("POST", "/api/v1/auth/logout", nil)
	if r.Status != 204 || len(r.Header.Values("Set-Cookie")) != 2 {
		t.Errorf("204 and two cookie deletions: %d %v", r.Status, r.Header.Values("Set-Cookie"))
	}
}

// --- request validation (extra-tags in openapi.yaml) ---

func TestSendCodeRejectedByValidationBeforeService(t *testing.T) {
	m := newMocks(t) // no EXPECT on the services: validation failures never reach them
	for name, body := range map[string]string{"empty object": `{}`, "empty email": `{"email":""}`} {
		r := m.do("POST", "/api/v1/auth/email/code", strings.NewReader(body), jsonBody)
		if r.Status != 400 || r.Body["error"] != "이메일 형식이 올바르지 않아요" {
			t.Errorf("%s: %d %v", name, r.Status, r.Body)
		}
	}
}

func TestConfirmRejectedByValidationBeforeService(t *testing.T) {
	m := newMocks(t)
	for name, body := range map[string]string{
		"code length": `{"email":"a@b.com","code":"12ab"}`,
		"no code":     `{"email":"a@b.com"}`,
		"not digits":  `{"email":"a@b.com","code":"abcdef"}`,
	} {
		r := m.do("POST", "/api/v1/auth/email/confirm", strings.NewReader(body), jsonBody)
		if r.Status != 400 || r.Body["error"] != "인증코드가 올바르지 않아요" {
			t.Errorf("%s: %d %v", name, r.Status, r.Body)
		}
	}
}

func TestConfirmTrimsCodeBeforeValidatingAndCallingService(t *testing.T) {
	m := newMocks(t)
	m.auth.EXPECT().Confirm(gomock.Any(), "a@b.com", "123456", false).Return(authsvcResult(), nil)
	r := m.do("POST", "/api/v1/auth/email/confirm", strings.NewReader(`{"email":"a@b.com","code":" 123456 "}`), jsonBody)
	if r.Status != 200 {
		t.Errorf("a code with surrounding whitespace must pass: %d %v", r.Status, r.Body)
	}
}

func authsvcResult() authsvc.ConfirmResult {
	now := time.Now()
	return authsvc.ConfirmResult{
		User:   user.User{ID: "u1", Email: "a@b.com"},
		Tokens: auth.Tokens{Access: "a", Refresh: "r", AccessExpiresAt: now.Add(time.Hour), RefreshExpiresAt: now.Add(time.Hour)},
	}
}

func TestRefreshTokenTooLongIsRejected(t *testing.T) {
	m := newMocks(t)
	long := strings.Repeat("a", 4097)
	r := m.do("POST", "/api/v1/auth/refresh", strings.NewReader(`{"refreshToken":"`+long+`"}`), jsonBody)
	if r.Status != 400 || r.Body["error"] != "refreshToken 값이 올바르지 않아요" {
		t.Errorf("token too long: %d %v", r.Status, r.Body)
	}
}

func TestListContractsRejectsUnknownStatus(t *testing.T) {
	m := newMocks(t)
	m.loggedIn()
	r := m.do("GET", "/api/v1/contracts?status=weird", nil, bearer)
	if r.Status != 400 || r.Body["error"] != "status 값이 올바르지 않아요" {
		t.Errorf("unknown status: %d %v", r.Status, r.Body)
	}
}
