package app_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/CleanSign/cleansign/server/internal/app"
	"github.com/CleanSign/cleansign/server/internal/config"
	contractsvc "github.com/CleanSign/cleansign/server/internal/service/contract"
	"github.com/CleanSign/cleansign/server/internal/testutil/pgtest"
)

func fastWorker(c *contractsvc.WorkerConfig) {
	c.PollInterval = 10 * time.Millisecond
	c.ReapInterval = 30 * time.Millisecond
	c.ShutdownWait = 5 * time.Second
}

// The API and the worker are separate processes sharing only the database: the API accepts the upload,
// the worker (here a WorkerApp, as `server worker` runs it) analyses it.
func TestSeparateWorkerProcessAnalysesWhatTheAPIQueued(t *testing.T) {
	e := newEnvWithoutWorker(t)
	token := e.login("a@b.com")
	id := e.uploadPDF(token)

	w := app.BuildWorker(e.app.PoolForTest(), config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		app.Overrides{Analyzer: e.analyzer, TuneWorker: fastWorker})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, "0") }() // port 0: any free port for the health endpoint

	if r := e.waitDone(token, id); r.Body["status"] != "completed" {
		t.Fatalf("a worker in another process finishes the job: %s", r.Raw)
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a clean stop is not an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker process did not stop")
	}
	if n := e.count(`SELECT count(*) FROM contract_file`); n != 0 {
		t.Errorf("the PDF is deleted: %d", n)
	}
}

func TestWorkerHealthFollowsTheDatabase(t *testing.T) {
	pool := pgtest.New(t)
	w := app.BuildWorker(pool, config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{})
	srv := httptest.NewServer(w.Health())
	defer srv.Close()

	get := func() int {
		res, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if got := get(); got != 200 {
		t.Errorf("healthy: %d", got)
	}
	pool.Close() // the database became unreachable
	if got := get(); got != 503 {
		t.Errorf("without the database the worker is not healthy: %d", got)
	}
}

func TestWorkerStopsWhenItCannotOpenItsHealthPort(t *testing.T) {
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port

	pool := pgtest.New(t)
	w := app.BuildWorker(pool, config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		app.Overrides{Analyzer: &fakeAnalyzer{}, TuneWorker: fastWorker})

	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background(), strconv.Itoa(port)) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a worker that cannot report its health must fail instead of running blind")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept going although the health port is taken")
	}
}
