package database_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CleanSign/cleansign/server/internal/database"
	"github.com/CleanSign/cleansign/server/internal/domain/auth"
	"github.com/CleanSign/cleansign/server/internal/domain/contract"
	"github.com/CleanSign/cleansign/server/internal/domain/user"
	"github.com/CleanSign/cleansign/server/internal/shared/errs"
	"github.com/CleanSign/cleansign/server/internal/testutil/pgtest"
)

var ctx = context.Background()

// Several processes (API replicas, workers) may start at the same moment on an empty database.
func TestMigrateIsSafeWhenRunConcurrently(t *testing.T) {
	pool := pgtest.NewUnmigrated(t)

	const starters = 8
	errs := make(chan error, starters)
	var wg sync.WaitGroup
	for i := 0; i < starters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- database.Migrate(ctx, pool)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("every concurrent Migrate must succeed: %v", err)
		}
	}

	var applied, versions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('users', 'contract', 'contract_file')`).Scan(&applied); err != nil || applied != 3 {
		t.Errorf("the schema is complete: %d tables %v", applied, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id > 0`).Scan(&versions); err != nil || versions != 3 {
		t.Errorf("each migration is recorded exactly once: %d %v", versions, err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := pgtest.New(t)
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatalf("두 번째 적용도 성공해야 한다: %v", err)
	}
}

func newUser(t *testing.T, repo *database.UserRepo, id, email string) user.User {
	t.Helper()
	u, err := repo.Create(ctx, user.User{ID: id, Email: email, EmailVerified: true})
	if err != nil {
		t.Fatalf("Create user: %v", err)
	}
	return u
}

func TestUserRepo(t *testing.T) {
	pool := pgtest.New(t)
	repo := database.NewUserRepo(pool)

	if _, err := repo.GetByID(ctx, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("없는 사용자: %v", err)
	}
	u := newUser(t, repo, "u1", "a@b.com")
	if u.CreatedAt.IsZero() || u.Name != "" || !u.EmailVerified {
		t.Errorf("생성 결과: %+v", u)
	}
	if got, err := repo.GetByEmail(ctx, "a@b.com"); err != nil || got.ID != "u1" {
		t.Errorf("GetByEmail: %+v %v", got, err)
	}
	if _, err := repo.Create(ctx, user.User{ID: "u2", Email: "a@b.com"}); err == nil {
		t.Error("같은 이메일은 중복 가입할 수 없다")
	}

	c, err := repo.CreateConsent(ctx, user.Consent{ID: "c1", UserID: "u1", Terms: true, Privacy: true, Marketing: false})
	if err != nil || c.UserID != "u1" || !c.Terms || c.Marketing || c.CreatedAt.IsZero() {
		t.Errorf("동의 저장: %+v %v", c, err)
	}
}

func TestCodeAndSessionRepos(t *testing.T) {
	pool := pgtest.New(t)
	users := database.NewUserRepo(pool)
	codes := database.NewCodeRepo(pool)
	sessions := database.NewSessionRepo(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	if _, err := codes.Get(ctx, "a@b.com"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("없는 코드: %v", err)
	}
	if err := codes.Upsert(ctx, auth.EmailCode{Email: "a@b.com", CodeHash: "h1", SentAt: now, ExpiresAt: now.Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	codes.IncrementAttempts(ctx, "a@b.com")
	codes.IncrementAttempts(ctx, "a@b.com")
	got, err := codes.Get(ctx, "a@b.com")
	if err != nil || got.Attempts != 2 || got.CodeHash != "h1" || !got.SentAt.Equal(now) {
		t.Errorf("코드 조회: %+v %v", got, err)
	}
	// resending overwrites and the attempt count resets to 0
	codes.Upsert(ctx, auth.EmailCode{Email: "a@b.com", CodeHash: "h2", SentAt: now, ExpiresAt: now.Add(5 * time.Minute)})
	if got, _ := codes.Get(ctx, "a@b.com"); got.CodeHash != "h2" || got.Attempts != 0 {
		t.Errorf("덮어쓰기: %+v", got)
	}
	codes.Upsert(ctx, auth.EmailCode{Email: "old@b.com", CodeHash: "x", SentAt: now, ExpiresAt: now.Add(-time.Hour)})
	if n, err := codes.DeleteExpired(ctx, now); err != nil || n != 1 {
		t.Errorf("만료된 코드 삭제: %d %v", n, err)
	}
	if err := codes.Delete(ctx, "a@b.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := codes.Get(ctx, "a@b.com"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("삭제 뒤: %v", err)
	}

	newUser(t, users, "u1", "a@b.com")
	s := auth.Session{ID: "s1", UserID: "u1", RememberDevice: true, ExpiresAt: now.Add(time.Hour)}
	if err := sessions.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got, err := sessions.Get(ctx, "s1"); err != nil || got.UserID != "u1" || !got.RememberDevice || !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Errorf("세션 조회: %+v %v", got, err)
	}
	sessions.Create(ctx, auth.Session{ID: "s2", UserID: "u1", ExpiresAt: now.Add(-time.Hour)})
	if n, _ := sessions.DeleteExpired(ctx, now); n != 1 {
		t.Errorf("만료된 세션 삭제: %d", n)
	}
	if err := sessions.Delete(ctx, "no-such"); err != nil {
		t.Errorf("없는 세션을 지워도 오류가 아니다: %v", err)
	}
	sessions.Create(ctx, auth.Session{ID: "s3", UserID: "u1", ExpiresAt: now.Add(time.Hour)})
	if err := sessions.DeleteByUser(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Get(ctx, "s1"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("전체 폐기 뒤: %v", err)
	}
}

func pending(id, userID string) contract.Contract {
	p := contract.InitialProgress()
	return contract.Contract{ID: id, UserID: userID, FileName: id + ".pdf", MimeType: "application/pdf", Progress: &p}
}

func clause(contractID string, i int32, law ...string) contract.Clause {
	typ, conf, lmax := "선순위근저당", float32(0.9), int32(100)
	return contract.Clause{
		ID: contractID + "-c" + string(rune('0'+i)), ContractID: contractID, Original: "원문", ClauseType: &typ, Confidence: &conf, LMax: &lmax,
		LossAmount: 40, Severity: contract.SeverityHigh, Reason: "사유", LawReference: law, SafeText: "대체", OrderIndex: i,
	}
}

func TestContractLifecycle(t *testing.T) {
	pool := pgtest.New(t)
	newUser(t, database.NewUserRepo(pool), "u1", "a@b.com")
	newUser(t, database.NewUserRepo(pool), "u2", "c@d.com")
	repo := database.NewContractRepo(pool)

	if err := repo.Create(ctx, pending("k1", "u1")); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, "u1", "k1")
	if err != nil || got.Status != contract.StatusPending || got.Progress == nil || got.Progress.Stage != "reading" {
		t.Fatalf("pending 조회: %+v %v", got, err)
	}
	if _, err := repo.Get(ctx, "u2", "k1"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("남의 계약서: %v", err)
	}

	if err := repo.UpdateProgress(ctx, "k1", contract.Progress{Stage: "classifying", Done: 3, Total: 8}); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get(ctx, "u1", "k1"); got.Progress.Stage != "classifying" || got.Progress.Done != 3 || got.Progress.Total != 8 {
		t.Errorf("진행 갱신: %+v", got.Progress)
	}

	deposit := int32(300)
	res := contract.Result{
		LossAmount: 1000, RiskScore: 72, Grade: contract.SeverityHigh, LossMin: 500, LossMax: 1500, DepositAmount: &deposit,
		ModelVersion: "m / p", Warnings: []string{"w1"}, RiskFlow: json.RawMessage(`{"nodes":[1,2]}`),
	}
	saved, err := repo.Complete(ctx, "k1", res, []contract.Clause{clause("k1", 0, "민법 제1조", "민법 제2조"), clause("k1", 1)})
	if err != nil || !saved {
		t.Fatalf("Complete: %v %v", saved, err)
	}
	got, _ = repo.Get(ctx, "u1", "k1")
	if got.Status != contract.StatusCompleted || got.Progress != nil || got.ErrorMessage != nil ||
		got.LossAmount != 1000 || *got.RiskScore != 72 || *got.Grade != contract.SeverityHigh ||
		*got.LossMin != 500 || *got.LossMax != 1500 || *got.DepositAmount != 300 || *got.ModelVersion != "m / p" {
		t.Errorf("완료 상태: %+v", got)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "w1" || string(got.RiskFlow) != `{"nodes": [1, 2]}` && string(got.RiskFlow) != `{"nodes":[1,2]}` {
		t.Errorf("jsonb: %v %s", got.Warnings, got.RiskFlow)
	}

	clauses, err := repo.Clauses(ctx, "k1")
	if err != nil || len(clauses) != 2 {
		t.Fatalf("조항: %v %v", clauses, err)
	}
	if clauses[0].OrderIndex != 0 || len(clauses[0].LawReference) != 2 || clauses[0].LawReference[1] != "민법 제2조" {
		t.Errorf("첫 조항(법령은 쉼표로 이어 저장했다가 다시 나뉜다): %+v", clauses[0])
	}
	if clauses[1].LawReference != nil {
		t.Errorf("법령이 없으면 nil: %#v", clauses[1].LawReference)
	}
	if *clauses[0].Confidence != 0.9 {
		t.Errorf("confidence: %v", *clauses[0].Confidence)
	}

	// deleting also deletes the clauses
	if ok, err := repo.Delete(ctx, "u2", "k1"); err != nil || ok {
		t.Errorf("남의 계약서 삭제: %v %v", ok, err)
	}
	if ok, err := repo.Delete(ctx, "u1", "k1"); err != nil || !ok {
		t.Errorf("삭제: %v %v", ok, err)
	}
	if left, _ := repo.Clauses(ctx, "k1"); len(left) != 0 {
		t.Errorf("조항이 함께 지워져야 한다: %d", len(left))
	}
}

func TestCompleteDiscardedWhenDeleted(t *testing.T) {
	pool := pgtest.New(t)
	newUser(t, database.NewUserRepo(pool), "u1", "a@b.com")
	repo := database.NewContractRepo(pool)
	repo.Create(ctx, pending("k1", "u1"))
	repo.Delete(ctx, "u1", "k1")

	saved, err := repo.Complete(ctx, "k1", contract.Result{Grade: contract.SeverityLow}, []contract.Clause{clause("k1", 0)})
	if err != nil || saved {
		t.Fatalf("지워진 계약서는 저장하지 않는다: %v %v", saved, err)
	}
	if left, _ := repo.Clauses(ctx, "k1"); len(left) != 0 {
		t.Error("조항을 저장하면 안 된다")
	}
}

func TestCompleteIsAtomic(t *testing.T) {
	pool := pgtest.New(t)
	newUser(t, database.NewUserRepo(pool), "u1", "a@b.com")
	repo := database.NewContractRepo(pool)
	repo.Create(ctx, pending("k1", "u1"))

	bad := clause("k1", 0)
	bad.Severity = "unknown-severity" // an enum violation makes the clause save fail
	if saved, err := repo.Complete(ctx, "k1", contract.Result{Grade: contract.SeverityLow}, []contract.Clause{bad}); err == nil || saved {
		t.Fatalf("실패해야 한다: %v %v", saved, err)
	}
	if got, _ := repo.Get(ctx, "u1", "k1"); got.Status != contract.StatusPending {
		t.Errorf("트랜잭션이 롤백돼 pending으로 남아야 한다: %v", got.Status)
	}
}

func TestFail(t *testing.T) {
	pool := pgtest.New(t)
	newUser(t, database.NewUserRepo(pool), "u1", "a@b.com")
	repo := database.NewContractRepo(pool)
	repo.Create(ctx, pending("k1", "u1"))

	if err := repo.Fail(ctx, "k1", "실패했어요"); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.Get(ctx, "u1", "k1")
	if got.Status != contract.StatusFailed || got.ErrorMessage == nil || *got.ErrorMessage != "실패했어요" || got.Progress != nil {
		t.Errorf("failed 상태: %+v", got)
	}
}

func TestPageAndRecent(t *testing.T) {
	pool := pgtest.New(t)
	newUser(t, database.NewUserRepo(pool), "u1", "a@b.com")
	newUser(t, database.NewUserRepo(pool), "u2", "c@d.com")
	repo := database.NewContractRepo(pool)

	// create in this order: k1 completed (2 clauses) -> k2 failed -> k3 in progress (newest first: k3, k2, k1)
	for _, id := range []string{"k1", "k2", "k3"} {
		repo.Create(ctx, pending(id, "u1"))
		time.Sleep(5 * time.Millisecond)
	}
	repo.Create(ctx, pending("other", "u2"))
	repo.Complete(ctx, "k1", contract.Result{Grade: contract.SeverityHigh, LossAmount: 77}, []contract.Clause{clause("k1", 0), clause("k1", 1)})
	repo.Fail(ctx, "k2", "실패")

	page, err := repo.Page(ctx, "u1", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || !page.HasMore || len(page.Items) != 2 || page.Items[0].ID != "k3" || page.Items[1].ID != "k2" {
		t.Errorf("1쪽(2개): %+v", page)
	}
	if page.Items[1].ErrorMessage == nil || *page.Items[1].ErrorMessage != "실패" {
		t.Errorf("실패한 분석도 목록에 포함하고 사유를 담는다: %+v", page.Items[1])
	}
	page2, _ := repo.Page(ctx, "u1", 2, 2)
	if len(page2.Items) != 1 || page2.Items[0].ID != "k1" || page2.HasMore {
		t.Errorf("2쪽: %+v", page2)
	}
	if page2.Items[0].ClauseCount != 2 || page2.Items[0].LossAmount != 77 {
		t.Errorf("조항 수와 손실: %+v", page2.Items[0])
	}

	recent, err := repo.Recent(ctx, "u1", 3)
	if err != nil || len(recent) != 2 || recent[0].ID != "k3" || recent[1].ID != "k1" {
		t.Errorf("최근 목록은 실패를 제외한다: %+v %v", recent, err)
	}
	if one, _ := repo.Recent(ctx, "u1", 1); len(one) != 1 {
		t.Errorf("limit: %d", len(one))
	}
}

func TestOpenWithRetryWaitsForADatabaseThatIsStillStarting(t *testing.T) {
	pgtest.NewUnmigrated(t) // skips the test when there is no Postgres to run against

	admin, err := pgx.Connect(ctx, pgtest.AdminURL())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	name := "cleansign_retry_" + strings.ReplaceAll(t.Name(), "/", "_")[:20] + fmt.Sprint(time.Now().UnixNano()%100000)
	name = strings.ToLower(name)
	defer func() { _, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") }()

	u, _ := url.Parse(pgtest.AdminURL())
	u.Path = "/" + name

	// the database does not exist yet; it "starts" a little later
	go func() {
		time.Sleep(1500 * time.Millisecond)
		_, _ = admin.Exec(ctx, "CREATE DATABASE "+name)
	}()
	var retries int
	pool, err := database.OpenWithRetry(ctx, u.String(), 30*time.Second, func(error) { retries++ })
	if err != nil {
		t.Fatalf("it should connect once the database is there: %v", err)
	}
	pool.Close()
	if retries == 0 {
		t.Error("it should have had to wait at least once")
	}
}

func TestOpenWithRetryGivesUpAfterTheWaitAndRejectsABadURLAtOnce(t *testing.T) {
	start := time.Now()
	var retries int
	// nothing listens on port 1: it is retried until the wait is over, then reported
	_, err := database.OpenWithRetry(ctx, "postgres://postgres:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", 3*time.Second, func(error) { retries++ })
	if err == nil {
		t.Fatal("an unreachable database must end in an error")
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second || elapsed > 10*time.Second {
		t.Errorf("it should wait about the given time, took %v", elapsed)
	}
	if retries == 0 {
		t.Error("it should have retried at least once")
	}

	retries = 0
	start = time.Now()
	if _, err := database.OpenWithRetry(ctx, "not a url \x7f", time.Minute, func(error) { retries++ }); err == nil {
		t.Fatal("a malformed URL must fail")
	}
	if retries != 0 || time.Since(start) > time.Second {
		t.Errorf("a malformed URL is a settings mistake: no waiting (retries %d, took %v)", retries, time.Since(start))
	}
}
