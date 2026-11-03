package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CleanSign/cleansign/server/internal/app"
	"github.com/CleanSign/cleansign/server/internal/config"
	"github.com/CleanSign/cleansign/server/internal/domain/contract"
	contractsvc "github.com/CleanSign/cleansign/server/internal/service/contract"
	"github.com/CleanSign/cleansign/server/internal/testutil/pgtest"
)

// --- Fake external integrations ---

type fakeMailer struct {
	mu   sync.Mutex
	code map[string]string
}

func (m *fakeMailer) SendCode(_ context.Context, to, code string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.code[to] = code
	return nil
}

// last returns the last code sent to the address the server normalized (lowercased).
func (m *fakeMailer) last(to string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.code[strings.ToLower(to)]
}

type fakeAnalyzer struct {
	mu     sync.Mutex
	result contract.Analysis
	err    error
	gate   chan struct{}
}

func (a *fakeAnalyzer) Analyze(ctx context.Context, _ string, _ []byte, onProgress contractsvc.ProgressFunc) (contract.Analysis, error) {
	if err := onProgress(contract.Progress{Stage: "classifying", Done: 1, Total: 3}); err != nil {
		return contract.Analysis{}, err
	}
	a.mu.Lock()
	gate, res, err := a.gate, a.result, a.err
	a.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return contract.Analysis{}, ctx.Err()
		}
	}
	return res, err
}

func (a *fakeAnalyzer) set(res contract.Analysis, err error, gate chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.result, a.err, a.gate = res, err, gate
}

// --- Test environment ---

type env struct {
	t        *testing.T
	srv      *httptest.Server
	mail     *fakeMailer
	analyzer *fakeAnalyzer
	app      *app.App

	stopWorker context.CancelFunc // set while a worker is running
	workerDone chan struct{}
}

// newEnv starts the server together with a worker that polls fast.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := newEnvWithoutWorker(t)
	e.startWorker()
	return e
}

// newEnvWithoutWorker starts only the API server: uploads wait in the queue until a worker is started.
func newEnvWithoutWorker(t *testing.T) *env {
	t.Helper()
	pool := pgtest.New(t)
	e := &env{t: t, mail: &fakeMailer{code: map[string]string{}}, analyzer: &fakeAnalyzer{result: sampleAnalysis()}}
	cfg := config.Config{JWTSecret: "test-secret-at-least-16-chars"}
	e.app = app.Build(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{
		Analyzer: e.analyzer,
		Mailer:   e.mail,
		TuneWorker: func(c *contractsvc.WorkerConfig) {
			c.PollInterval = 10 * time.Millisecond
			c.ReapInterval = 30 * time.Millisecond
			c.ShutdownWait = 5 * time.Second
		},
	})
	e.srv = httptest.NewServer(e.app.Echo)
	t.Cleanup(func() {
		e.srv.Close()
		e.stopWorkerAndWait()
	})
	return e
}

func (e *env) startWorker() {
	e.t.Helper()
	if e.stopWorker != nil {
		e.t.Fatal("the worker is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.app.Worker.Run(ctx)
	}()
	e.stopWorker, e.workerDone = cancel, done
}

// stopWorkerAndWait stops the worker and waits until its running analysis (if any) is over.
func (e *env) stopWorkerAndWait() {
	if e.stopWorker == nil {
		return
	}
	e.stopWorker()
	select {
	case <-e.workerDone:
	case <-time.After(10 * time.Second):
		e.t.Error("the worker did not stop")
	}
	e.stopWorker = nil
}

// sql runs a statement directly on the database, e.g. to make a job look old or its worker look dead.
func (e *env) sql(query string, args ...any) {
	e.t.Helper()
	if _, err := e.app.PoolForTest().Exec(context.Background(), query, args...); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.app.PoolForTest().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// eventually polls cond for up to 5 seconds.
func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for: %s", what)
}

func sampleAnalysis() contract.Analysis {
	deposit := int64(300_000_000)
	return contract.Analysis{
		RiskScore: 72, Grade: contract.SeverityHigh, LossAmount: 50_000_000, LossMin: 20_000_000, LossMax: 90_000_000,
		Deposit: &deposit, ModelVersion: "gemma4:12b", PolicyVersion: "2026.10",
		Clauses: []contract.AnalysisClause{
			{Original: "첫 조항", ClauseType: "선순위근저당", Confidence: 0.9, Severity: contract.SeverityHigh, LMax: 100, ExpectedLoss: 40_000_000, Reason: "사유1", LawReference: []string{"민법 제1조", "민법 제2조"}, SafeText: "대체1"},
			{Original: "둘째 조항", ClauseType: "기타불이익", Confidence: 0.5, Severity: contract.SeverityLow, LMax: 5, ExpectedLoss: 10_000_000, Reason: "사유2", SafeText: "대체2"},
		},
	}
}

type resp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

func (e *env) do(method, path, token string, body io.Reader, contentType string, mods ...func(*http.Request)) resp {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, m := range mods {
		m(req)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Header: res.Header, Raw: raw}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func (e *env) json(method, path, token string, payload any) resp {
	e.t.Helper()
	b, _ := json.Marshal(payload)
	return e.do(method, path, token, bytes.NewReader(b), "application/json")
}

func (e *env) get(path, token string) resp { return e.do("GET", path, token, nil, "") }

// login signs up/logs in by email and returns the access token.
func (e *env) login(email string) string {
	e.t.Helper()
	if r := e.json("POST", "/api/v1/auth/email/code", "", map[string]any{"email": email}); r.Status != 200 {
		e.t.Fatalf("코드 요청: %d %s", r.Status, r.Raw)
	}
	r := e.json("POST", "/api/v1/auth/email/confirm", "", map[string]any{"email": email, "code": e.mail.last(email)})
	if r.Status != 200 {
		e.t.Fatalf("로그인: %d %s", r.Status, r.Raw)
	}
	return r.Body["accessToken"].(string)
}

func (e *env) upload(token, field, filename, mime string, data []byte) resp {
	e.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="`+field+`"; filename="`+filename+`"`)
	h.Set("Content-Type", mime)
	part, _ := w.CreatePart(h)
	part.Write(data)
	w.Close()
	return e.do("POST", "/api/v1/contracts", token, &buf, w.FormDataContentType())
}

func (e *env) uploadPDF(token string) string {
	e.t.Helper()
	r := e.upload(token, "file", "lease.pdf", "application/pdf", []byte("%PDF-1.4 test"))
	if r.Status != 202 {
		e.t.Fatalf("업로드: %d %s", r.Status, r.Raw)
	}
	return r.Body["id"].(string)
}

// waitDone waits until the analysis leaves pending.
func (e *env) waitDone(token, id string) resp {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r := e.get("/api/v1/contracts/"+id, token)
		if r.Status == 200 && r.Body["status"] != "pending" {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("분석이 끝나지 않았다\n%s", e.queueState(id))
	return resp{}
}

// queueState describes where a contract is in the queue, for failure messages.
func (e *env) queueState(id string) string {
	var status string
	var attempts int
	var leaseUntil *time.Time
	var lockedBy *string
	var nextRun, dbNow time.Time
	err := e.app.PoolForTest().QueryRow(context.Background(),
		`SELECT status::text, attempts, lease_until, locked_by, next_run_at, now() FROM contract WHERE id = $1`, id).
		Scan(&status, &attempts, &leaseUntil, &lockedBy, &nextRun, &dbNow)
	if err != nil {
		return "queue state unavailable: " + err.Error()
	}
	return fmt.Sprintf("queue state: status=%s attempts=%d lease_until=%v locked_by=%v next_run_at=%v | db now=%v, test now=%v",
		status, attempts, leaseUntil, lockedBy, nextRun, dbNow, time.Now())
}

func wantErr(t *testing.T, r resp, status int, msg string) {
	t.Helper()
	if r.Status != status || r.Body["error"] != msg {
		t.Errorf("상태 %d, 메시지 %q를 기대했지만 %d %s", status, msg, r.Status, r.Raw)
	}
}

// --- Auth ---

func TestAuthFlow(t *testing.T) {
	e := newEnv(t)

	wantErr(t, e.json("POST", "/api/v1/auth/email/code", "", map[string]any{"email": "nope"}), 400, "이메일 형식이 올바르지 않아요")
	wantErr(t, e.do("POST", "/api/v1/auth/email/code", "", strings.NewReader("{bad"), "application/json"), 400, "요청 형식이 올바르지 않아요")

	r := e.json("POST", "/api/v1/auth/email/code", "", map[string]any{"email": "User@Example.com"})
	if r.Status != 200 || r.Body["retryAfterSeconds"] != float64(60) {
		t.Fatalf("코드 요청: %d %s", r.Status, r.Raw)
	}
	code := e.mail.last("user@example.com")
	if len(code) != 6 {
		t.Fatalf("6자리 코드를 보내야 한다: %q", code)
	}

	r = e.json("POST", "/api/v1/auth/email/code", "", map[string]any{"email": "user@example.com"})
	if r.Status != 429 || r.Header.Get("Retry-After") == "" {
		t.Errorf("쿨다운 안의 재요청은 429와 Retry-After: %d %v", r.Status, r.Header)
	}

	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	wantErr(t, e.json("POST", "/api/v1/auth/email/confirm", "", map[string]any{"email": "user@example.com", "code": wrong}), 400, "인증코드가 올바르지 않아요")

	r = e.json("POST", "/api/v1/auth/email/confirm", "", map[string]any{"email": "user@example.com", "code": code, "rememberDevice": true})
	if r.Status != 200 || r.Body["accessToken"] == "" || r.Body["refreshToken"] == "" {
		t.Fatalf("로그인: %d %s", r.Status, r.Raw)
	}
	cookies := r.Header.Values("Set-Cookie")
	if len(cookies) != 2 {
		t.Fatalf("쿠키 두 개(access, refresh)가 내려가야 한다: %v", cookies)
	}
	for _, c := range cookies {
		if !strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Lax") || !strings.Contains(c, "Path=/") {
			t.Errorf("쿠키 속성: %s", c)
		}
	}
	access, refresh := r.Body["accessToken"].(string), r.Body["refreshToken"].(string)

	// a code can be used only once
	wantErr(t, e.json("POST", "/api/v1/auth/email/confirm", "", map[string]any{"email": "user@example.com", "code": code}), 400, "인증코드가 만료됐어요. 다시 받아 주세요")

	// Both Bearer and cookie authenticate. If a header is present, it does not fall through to the cookie
	me := e.get("/api/v1/users/me", access)
	if me.Status != 200 || me.Body["email"] != "user@example.com" || me.Body["emailVerified"] != true || me.Body["name"] != "" || me.Body["id"] == "" {
		t.Errorf("내 정보: %d %s", me.Status, me.Raw)
	}
	viaCookie := e.do("GET", "/api/v1/users/me", "", nil, "", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "cs_access", Value: access}) })
	if viaCookie.Status != 200 {
		t.Errorf("쿠키 인증: %d", viaCookie.Status)
	}
	badHeaderGoodCookie := e.do("GET", "/api/v1/users/me", "garbage", nil, "", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "cs_access", Value: access}) })
	if badHeaderGoodCookie.Status != 401 {
		t.Errorf("틀린 Authorization 헤더가 있으면 쿠키로 넘어가지 않는다: %d", badHeaderGoodCookie.Status)
	}
	wantErr(t, e.get("/api/v1/users/me", ""), 401, "unauthorized")
	wantErr(t, e.get("/api/v1/users/me", refresh), 401, "unauthorized") // the API cannot be used with a refresh token

	// refresh: works via cookie or body. The refresh token stays the same
	byCookie := e.do("POST", "/api/v1/auth/refresh", "", nil, "", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "cs_refresh", Value: refresh}) })
	if byCookie.Status != 200 || byCookie.Body["refreshToken"] != refresh || byCookie.Body["accessToken"] == "" {
		t.Errorf("쿠키로 갱신: %d %s", byCookie.Status, byCookie.Raw)
	}
	byBody := e.json("POST", "/api/v1/auth/refresh", "", map[string]any{"refreshToken": refresh})
	if byBody.Status != 200 {
		t.Errorf("본문으로 갱신: %d %s", byBody.Status, byBody.Raw)
	}
	wantErr(t, e.json("POST", "/api/v1/auth/refresh", "", map[string]any{"refreshToken": access}), 401, "unauthorized")
	wantErr(t, e.do("POST", "/api/v1/auth/refresh", "", nil, ""), 401, "unauthorized")

	// after logout the session is revoked, so neither token can be used
	out := e.do("POST", "/api/v1/auth/logout", access, nil, "")
	if out.Status != 204 {
		t.Fatalf("로그아웃: %d", out.Status)
	}
	if got := out.Header.Values("Set-Cookie"); len(got) != 2 || !strings.Contains(got[0], "Max-Age=0") {
		t.Errorf("쿠키를 지워야 한다: %v", got)
	}
	wantErr(t, e.get("/api/v1/users/me", access), 401, "unauthorized")
	wantErr(t, e.json("POST", "/api/v1/auth/refresh", "", map[string]any{"refreshToken": refresh}), 401, "unauthorized")
	if r := e.do("POST", "/api/v1/auth/logout", "", nil, ""); r.Status != 204 {
		t.Errorf("로그인하지 않아도 로그아웃은 204: %d", r.Status)
	}
}

func TestSameEmailKeepsSameUser(t *testing.T) {
	e := newEnv(t)
	first := e.get("/api/v1/users/me", e.login("same@x.com")).Body["id"]
	// logging in again (even with different letter case) does not sign up anew; it is the same user
	second := e.get("/api/v1/users/me", e.login("SAME@x.com")).Body["id"]
	if first == nil || first != second {
		t.Errorf("같은 이메일은 같은 사용자여야 한다: %v vs %v", first, second)
	}
}

// --- Users / consent ---

func TestConsent(t *testing.T) {
	e := newEnv(t)
	token := e.login("a@b.com")
	post := func(body any) resp { return e.json("POST", "/api/v1/users/me/consents", token, body) }

	wantErr(t, e.json("POST", "/api/v1/users/me/consents", "", map[string]any{"terms": true, "privacy": true}), 401, "unauthorized")
	wantErr(t, post(map[string]any{"terms": "yes", "privacy": true}), 400, "terms, privacy는 boolean이어야 합니다")
	wantErr(t, post(map[string]any{"privacy": true}), 400, "terms, privacy는 boolean이어야 합니다")
	wantErr(t, e.do("POST", "/api/v1/users/me/consents", token, strings.NewReader("not json"), "application/json"), 400, "terms, privacy는 boolean이어야 합니다")
	wantErr(t, post(map[string]any{"terms": true, "privacy": false}), 400, "이용약관과 개인정보처리방침 동의는 필수입니다")
	wantErr(t, post(map[string]any{"terms": false, "privacy": false}), 400, "이용약관과 개인정보처리방침 동의는 필수입니다")

	r := post(map[string]any{"terms": true, "privacy": true})
	if r.Status != 201 || r.Body["marketing"] != false || r.Body["terms"] != true || r.Body["privacy"] != true || r.Body["id"] == "" || r.Body["userId"] == "" || r.Body["createdAt"] == "" {
		t.Errorf("marketing 기본값 false: %d %s", r.Status, r.Raw)
	}
	r = post(map[string]any{"terms": true, "privacy": true, "marketing": true})
	if r.Status != 201 || r.Body["marketing"] != true {
		t.Errorf("marketing 동의: %d %s", r.Status, r.Raw)
	}
	r = post(map[string]any{"terms": true, "privacy": true, "marketing": "x"})
	if r.Status != 201 || r.Body["marketing"] != false {
		t.Errorf("marketing이 boolean이 아니면 false: %d %s", r.Status, r.Raw)
	}
}

// --- Contracts ---

func TestUploadValidation(t *testing.T) {
	e := newEnv(t)
	token := e.login("a@b.com")

	wantErr(t, e.upload("", "file", "a.pdf", "application/pdf", []byte("%PDF")), 401, "unauthorized")
	wantErr(t, e.upload(token, "other", "a.pdf", "application/pdf", []byte("%PDF")), 400, "file이 필요합니다")
	wantErr(t, e.do("POST", "/api/v1/contracts", token, strings.NewReader("x"), "application/json"), 400, "file이 필요합니다")
	wantErr(t, e.upload(token, "file", "a.txt", "text/plain", []byte("hello")), 415, "PDF 파일만 올릴 수 있어요")
	wantErr(t, e.upload(token, "file", "a.pdf", "application/pdf", nil), 413, "파일 크기는 10MB 이하여야 해요")
	wantErr(t, e.upload(token, "file", "big.pdf", "application/pdf", make([]byte, contract.MaxUploadBytes+1)), 413, "파일 크기는 10MB 이하여야 해요")
	wantErr(t, e.upload(token, "file", "huge.pdf", "application/pdf", make([]byte, contract.MaxUploadBytes+(2<<20))), 413, "파일 크기는 10MB 이하여야 해요")

	// accept if either the extension or the MIME type is PDF
	if r := e.upload(token, "file", "scan.PDF", "application/octet-stream", []byte("%PDF")); r.Status != 202 {
		t.Errorf("확장자로 판별: %d %s", r.Status, r.Raw)
	}
	if r := e.upload(token, "file", "noext", "application/pdf", []byte("%PDF")); r.Status != 202 {
		t.Errorf("MIME으로 판별: %d %s", r.Status, r.Raw)
	}
}

func TestContractCompletedShape(t *testing.T) {
	e := newEnv(t)
	token := e.login("a@b.com")

	gate := make(chan struct{})
	e.analyzer.set(sampleAnalysis(), nil, gate)
	id := e.uploadPDF(token)

	// analyzing: 200, and only progress has a value. The other result fields are null but the keys exist
	pend := e.get("/api/v1/contracts/"+id, token)
	if pend.Status != 200 || pend.Body["status"] != "pending" || pend.Body["error"] != nil {
		t.Fatalf("pending 상세: %d %s", pend.Status, pend.Raw)
	}
	for _, k := range []string{"riskScore", "grade", "lossRange", "depositAmount", "flow"} {
		if v, ok := pend.Body[k]; !ok || v != nil {
			t.Errorf("pending의 %s는 null이어야 한다: %v (키 있음=%v)", k, v, ok)
		}
	}
	if p, _ := pend.Body["progress"].(map[string]any); p == nil || p["stage"] == "" {
		t.Errorf("pending이면 progress가 있다: %s", pend.Raw)
	}
	if w, ok := pend.Body["warnings"].([]any); !ok || len(w) != 0 {
		t.Errorf("warnings 기본값 []: %v", pend.Body["warnings"])
	}
	if items, ok := pend.Body["riskItems"].([]any); !ok || len(items) != 0 {
		t.Errorf("riskItems 기본값 []: %v", pend.Body["riskItems"])
	}
	if r := e.get("/api/v1/contracts/"+id+"/clauses/x", token); r.Status != 404 {
		t.Errorf("완료 전 조항 조회는 404: %d", r.Status)
	}

	close(gate)
	done := e.waitDone(token, id)
	b := done.Body
	if b["status"] != "completed" || b["progress"] != nil || b["error"] != nil {
		t.Fatalf("completed 상태: %s", done.Raw)
	}
	if b["riskScore"] != float64(72) || b["grade"] != "high" || b["lossAmount"] != float64(50_000_000) || b["depositAmount"] != float64(300_000_000) {
		t.Errorf("결과 값: %s", done.Raw)
	}
	if lr, _ := b["lossRange"].([]any); len(lr) != 2 || lr[0] != float64(20_000_000) || lr[1] != float64(90_000_000) {
		t.Errorf("lossRange: %v", b["lossRange"])
	}
	if b["flow"] != nil || b["fileName"] != "lease.pdf" {
		t.Errorf("flow/fileName: %v %v", b["flow"], b["fileName"])
	}

	items := b["riskItems"].([]any)
	if len(items) != 2 {
		t.Fatalf("조항 수: %d", len(items))
	}
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if first["original"] != "첫 조항" || second["original"] != "둘째 조항" {
		t.Errorf("orderIndex 순: %v / %v", first["original"], second["original"])
	}
	if first["lawReference"] != "민법 제1조, 민법 제2조" {
		t.Errorf("상세 응답의 lawReference는 쉼표로 이은 문자열이다: %#v", first["lawReference"])
	}
	if v, ok := second["lawReference"]; !ok || v != nil {
		t.Errorf("법령이 없으면 null: %#v", v)
	}
	if first["confidence"] != 0.9 || first["severity"] != "high" || first["clauseType"] != "선순위근저당" {
		t.Errorf("조항 값: %v", first)
	}

	// clause detail: lawReference is an array
	clauseID := first["id"].(string)
	cl := e.get("/api/v1/contracts/"+id+"/clauses/"+clauseID, token)
	if cl.Status != 200 || cl.Body["contractId"] != id || cl.Body["safeText"] != "대체1" {
		t.Fatalf("조항 상세: %d %s", cl.Status, cl.Raw)
	}
	if law, _ := cl.Body["lawReference"].([]any); len(law) != 2 || law[1] != "민법 제2조" {
		t.Errorf("조항 상세의 lawReference는 배열: %v", cl.Body["lawReference"])
	}
	cl2 := e.get("/api/v1/contracts/"+id+"/clauses/"+second["id"].(string), token)
	if law, ok := cl2.Body["lawReference"].([]any); !ok || len(law) != 0 {
		t.Errorf("법령이 없으면 []: %#v", cl2.Body["lawReference"])
	}
	wantErr(t, e.get("/api/v1/contracts/"+id+"/clauses/nope", token), 404, "not found")
}

func TestContractFailedShape(t *testing.T) {
	e := newEnv(t)
	token := e.login("a@b.com")
	e.analyzer.set(contract.Analysis{}, &contractsvc.AnalysisError{Message: "글자를 읽지 못했어요"}, nil)
	id := e.uploadPDF(token)

	done := e.waitDone(token, id)
	if done.Body["status"] != "failed" || done.Body["error"] != "글자를 읽지 못했어요" || done.Body["progress"] != nil {
		t.Errorf("failed 상태: %s", done.Raw)
	}
	wantErr(t, e.get("/api/v1/contracts/"+id+"/clauses/x", token), 404, "not found")
}

func TestOwnershipIsolation(t *testing.T) {
	e := newEnv(t)
	owner, other := e.login("owner@x.com"), e.login("other@x.com")
	id := e.uploadPDF(owner)
	done := e.waitDone(owner, id)
	clauseID := done.Body["riskItems"].([]any)[0].(map[string]any)["id"].(string)

	wantErr(t, e.get("/api/v1/contracts/"+id, other), 404, "not found")
	wantErr(t, e.get("/api/v1/contracts/"+id+"/clauses/"+clauseID, other), 404, "not found")
	wantErr(t, e.do("DELETE", "/api/v1/contracts/"+id, other, nil, ""), 404, "not found")
	if r := e.get("/api/v1/contracts", other); r.Body["total"] != float64(0) {
		t.Errorf("남의 계약서는 목록에도 없다: %s", r.Raw)
	}
	if r := e.get("/api/v1/contracts/"+id, owner); r.Status != 200 {
		t.Errorf("소유자는 그대로 볼 수 있다: %d", r.Status)
	}
}

func TestUnauthenticatedContractEndpoints(t *testing.T) {
	e := newEnv(t)
	for _, c := range [][2]string{
		{"GET", "/api/v1/contracts"}, {"GET", "/api/v1/contracts/x"}, {"DELETE", "/api/v1/contracts/x"}, {"GET", "/api/v1/contracts/x/clauses/y"},
	} {
		wantErr(t, e.do(c[0], c[1], "", nil, ""), 401, "unauthorized")
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	token := e.login("a@b.com")
	id := e.uploadPDF(token)
	e.waitDone(token, id)

	if r := e.do("DELETE", "/api/v1/contracts/"+id, token, nil, ""); r.Status != 204 || len(r.Raw) != 0 {
		t.Errorf("삭제: %d %q", r.Status, r.Raw)
	}
	wantErr(t, e.get("/api/v1/contracts/"+id, token), 404, "not found")
	wantErr(t, e.do("DELETE", "/api/v1/contracts/"+id, token, nil, ""), 404, "not found")
}

func TestDeleteDuringAnalysisDiscardsResult(t *testing.T) {
	e := newEnv(t)
	token := e.login("a@b.com")
	gate := make(chan struct{})
	e.analyzer.set(sampleAnalysis(), nil, gate)
	id := e.uploadPDF(token)

	if r := e.do("DELETE", "/api/v1/contracts/"+id, token, nil, ""); r.Status != 204 {
		t.Fatalf("분석 중 삭제: %d", r.Status)
	}
	close(gate)
	e.stopWorkerAndWait() // wait until the worker has dealt with the finished analysis
	wantErr(t, e.get("/api/v1/contracts/"+id, token), 404, "not found")
	if n := e.count(`SELECT count(*) FROM contract_danger_clause`); n != 0 {
		t.Errorf("지운 계약서의 조항이 저장되면 안 된다: %d", n)
	}
}

func TestListContracts(t *testing.T) {
	e := newEnv(t)
	token := e.login("a@b.com")

	// create in this order: completed (2 clauses) -> failed -> in progress
	done := e.uploadPDF(token)
	e.waitDone(token, done)
	e.analyzer.set(contract.Analysis{}, &contractsvc.AnalysisError{Message: "실패 사유"}, nil)
	failed := e.uploadPDF(token)
	e.waitDone(token, failed)
	gate := make(chan struct{})
	e.analyzer.set(sampleAnalysis(), nil, gate)
	pending := e.uploadPDF(token)
	t.Cleanup(func() { close(gate) })

	r := e.get("/api/v1/contracts", token)
	items := r.Body["items"].([]any)
	if r.Status != 200 || len(items) != 3 || r.Body["page"] != float64(1) || r.Body["total"] != float64(3) || r.Body["hasMore"] != false {
		t.Fatalf("목록: %d %s", r.Status, r.Raw)
	}
	byID := map[string]map[string]any{}
	for _, it := range items {
		m := it.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if items[0].(map[string]any)["id"] != pending || items[2].(map[string]any)["id"] != done {
		t.Errorf("최신순이어야 한다: %v", items)
	}
	if byID[done]["riskCount"] != float64(2) || byID[done]["error"] != nil || byID[done]["lossAmount"] != float64(50_000_000) {
		t.Errorf("완료 항목: %v", byID[done])
	}
	if v, ok := byID[failed]["riskCount"]; !ok || v != nil || byID[failed]["error"] != "실패 사유" {
		t.Errorf("실패 항목은 riskCount가 null이고 error가 있다: %v", byID[failed])
	}
	if v, ok := byID[pending]["riskCount"]; !ok || v != nil || byID[pending]["error"] != nil {
		t.Errorf("진행 중 항목: %v", byID[pending])
	}

	// pagination and clamping
	p1 := e.get("/api/v1/contracts?page=1&limit=2", token)
	if len(p1.Body["items"].([]any)) != 2 || p1.Body["hasMore"] != true {
		t.Errorf("1쪽: %s", p1.Raw)
	}
	p2 := e.get("/api/v1/contracts?page=2&limit=2", token)
	if len(p2.Body["items"].([]any)) != 1 || p2.Body["hasMore"] != false || p2.Body["page"] != float64(2) {
		t.Errorf("2쪽: %s", p2.Raw)
	}
	if r := e.get("/api/v1/contracts?page=abc&limit=0", token); r.Body["page"] != float64(1) || len(r.Body["items"].([]any)) != 3 {
		t.Errorf("잘못된 값은 기본값: %s", r.Raw)
	}
	if r := e.get("/api/v1/contracts?page=-3&limit=-5", token); r.Body["page"] != float64(1) || len(r.Body["items"].([]any)) != 1 {
		t.Errorf("음수는 1로 올린다(limit=1): %s", r.Raw)
	}
	if r := e.get("/api/v1/contracts?limit=1000", token); len(r.Body["items"].([]any)) != 3 {
		t.Errorf("limit 상한 50: %s", r.Raw)
	}

	// for the home screen: exclude failed
	active := e.get("/api/v1/contracts?status=active&limit=3", token)
	ai := active.Body["items"].([]any)
	if len(ai) != 2 || ai[0].(map[string]any)["id"] != pending || ai[1].(map[string]any)["id"] != done {
		t.Errorf("status=active는 완료·진행 중만: %s", active.Raw)
	}
	if one := e.get("/api/v1/contracts?status=active&limit=1", token); len(one.Body["items"].([]any)) != 1 {
		t.Errorf("limit 적용: %s", one.Raw)
	}
}

// --- The analysis queue, end to end (HTTP, real worker, real Postgres) ---

func TestUploadWaitsInTheQueueForAWorker(t *testing.T) {
	e := newEnvWithoutWorker(t) // the API server alone: nobody analyses yet
	token := e.login("a@b.com")
	id := e.uploadPDF(token)

	time.Sleep(100 * time.Millisecond)
	if r := e.get("/api/v1/contracts/"+id, token); r.Body["status"] != "pending" {
		t.Fatalf("worker가 없으면 pending으로 기다린다: %s", r.Raw)
	}
	if n := e.count(`SELECT count(*) FROM contract_file WHERE contract_id = $1`, id); n != 1 {
		t.Fatalf("기다리는 동안 PDF가 저장돼 있어야 한다: %d", n)
	}

	e.startWorker() // a worker shows up later (could be another process)
	if done := e.waitDone(token, id); done.Body["status"] != "completed" {
		t.Errorf("worker가 시작되면 처리된다: %s", done.Raw)
	}
}

func TestPDFIsDeletedOnceTheAnalysisEnds(t *testing.T) {
	e := newEnv(t)
	token := e.login("a@b.com")

	ok := e.uploadPDF(token)
	if done := e.waitDone(token, ok); done.Body["status"] != "completed" {
		t.Fatalf("완료: %s", done.Raw)
	}

	e.analyzer.set(contract.Analysis{}, &contractsvc.AnalysisError{Message: "글자를 읽지 못했어요"}, nil)
	bad := e.uploadPDF(token)
	if done := e.waitDone(token, bad); done.Body["status"] != "failed" {
		t.Fatalf("실패: %s", done.Raw)
	}

	e.stopWorkerAndWait()
	if n := e.count(`SELECT count(*) FROM contract_file`); n != 0 {
		t.Errorf("끝난 분석의 PDF는 남지 않는다: %d", n)
	}
}

// The scenario that used to lose the analysis for 15 minutes: the worker dies in the middle of a job.
func TestAnalysisSurvivesAWorkerThatDied(t *testing.T) {
	e := newEnvWithoutWorker(t)
	token := e.login("a@b.com")
	id := e.uploadPDF(token)

	// a worker claimed the job and then died: it holds a lease that has run out and never finished
	e.sql(`UPDATE contract SET attempts = 1, locked_by = 'dead-worker', lease_until = now() - interval '1 minute' WHERE id = $1`, id)

	e.startWorker()
	// the reaper takes the job back from the dead worker and schedules the retry
	e.eventually("the job is requeued", func() bool {
		return e.count(`SELECT count(*) FROM contract WHERE id = $1 AND lease_until IS NULL AND locked_by IS NULL AND next_run_at > now()`, id) == 1
	})
	if r := e.get("/api/v1/contracts/"+id, token); r.Body["status"] != "pending" {
		t.Fatalf("그동안 사용자에게는 계속 분석 중이다: %s", r.Raw)
	}

	e.sql(`UPDATE contract SET next_run_at = now() WHERE id = $1`, id) // skip the 30 second retry delay
	done := e.waitDone(token, id)
	if done.Body["status"] != "completed" {
		t.Fatalf("다른 worker(여기서는 새 worker)가 이어서 끝낸다: %s", done.Raw)
	}
	e.stopWorkerAndWait()
	if n := e.count(`SELECT attempts FROM contract WHERE id = $1`, id); n != 2 {
		t.Errorf("두 번째 시도에서 끝났다: attempts = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM contract_file`); n != 0 {
		t.Errorf("PDF는 지워진다: %d", n)
	}
}

func TestJobThatWaitedTooLongInTheQueueFails(t *testing.T) {
	e := newEnvWithoutWorker(t)
	token := e.login("a@b.com")
	id := e.uploadPDF(token)
	e.sql(`UPDATE contract SET created_at = now() - interval '25 hours' WHERE id = $1`, id)

	e.startWorker() // its reaper gives up on the old entry before anyone claims it
	r := e.waitDone(token, id)
	if r.Body["status"] != "failed" || r.Body["error"] != contract.QueueExpiredMessage {
		t.Errorf("오래 기다린 건 실패 처리된다: %s", r.Raw)
	}
	e.stopWorkerAndWait()
	if n := e.count(`SELECT count(*) FROM contract_file`); n != 0 {
		t.Errorf("PDF도 지워진다: %d", n)
	}
}

func TestHealthAndUnknownRoute(t *testing.T) {
	e := newEnv(t)
	if r := e.get("/healthz", ""); r.Status != 200 || r.Body["status"] != "ok" {
		t.Errorf("healthz: %d %s", r.Status, r.Raw)
	}
	r := e.get("/api/v1/nope", "")
	if r.Status != 404 || r.Body["error"] == nil {
		t.Errorf("없는 경로도 {error} 모양: %d %s", r.Status, r.Raw)
	}
}

func TestRequestValidationFromSpec(t *testing.T) {
	e := newEnv(t)
	token := e.login("a@b.com")

	// 알 수 없는 status는 400 (page/limit은 보정이라 400이 아니다)
	wantErr(t, e.get("/api/v1/contracts?status=weird", token), 400, "status 값이 올바르지 않아요")
	if r := e.get("/api/v1/contracts?page=abc&limit=-1&status=active", token); r.Status != 200 {
		t.Errorf("page/limit 보정, status=active: %d %s", r.Status, r.Raw)
	}
	// 인증 요청: 빈 값과 잘못된 모양의 코드는 service에 닿기 전에 400
	wantErr(t, e.json("POST", "/api/v1/auth/email/code", "", map[string]any{"email": ""}), 400, "이메일 형식이 올바르지 않아요")
	wantErr(t, e.json("POST", "/api/v1/auth/email/confirm", "", map[string]any{"email": "a@b.com", "code": "12ab"}), 400, "인증코드가 올바르지 않아요")
	// refresh 토큰이 비정상적으로 길면 400
	wantErr(t, e.json("POST", "/api/v1/auth/refresh", "", map[string]any{"refreshToken": strings.Repeat("x", 5000)}), 400, "refreshToken 값이 올바르지 않아요")
}
