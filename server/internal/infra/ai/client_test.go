package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	domain "github.com/CleanSign/cleansign/server/internal/domain/contract"
	svc "github.com/CleanSign/cleansign/server/internal/service/contract"
)

const resultLine = `{"type":"result","data":{"riskScore":72,"grade":"high","lossAmount":1234.6,"lossRange":[100.4,2000],` +
	`"facts":{"deposit":null,"monthlyRent":50,"leaseType":"월세"},` +
	`"clauses":[{"id":"c1","original":"원문","clauseType":"x","confidence":0.9,"severity":"medium","lMax":1234.6,"lMaxFormula":"f",` +
	`"expectedLoss":10.0,"reason":"r","lawReference":null,"precedents":[],"safeText":"s"}],` +
	`"flow":null,"warnings":null,"modelVersion":"m","policyVersion":"p","stats":{"a":1}}}`

func ndjson(lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	}
}

func analyze(t *testing.T, h http.HandlerFunc, onProgress svc.ProgressFunc) (domain.Analysis, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	return New(srv.URL+"/", "", 5*time.Second).Analyze(context.Background(), "a.pdf", []byte("%PDF"), onProgress)
}

func wantMsg(t *testing.T, err error, msg string) *svc.AnalysisError {
	t.Helper()
	var ae *svc.AnalysisError
	if !errors.As(err, &ae) {
		t.Fatalf("AnalysisError가 아님: %T %v", err, err)
	}
	if ae.Message != msg {
		t.Fatalf("message = %q, want %q", ae.Message, msg)
	}
	return ae
}

func TestAnalyzeSuccess(t *testing.T) {
	var got []domain.Progress
	a, err := analyze(t, ndjson(
		`{"type":"progress","stage":"reading","done":0,"total":0}`,
		``,
		`{"type":"progress","stage":"scoring","done":2,"total":3}`,
		resultLine,
	), func(p domain.Progress) error { got = append(got, p); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Stage != "reading" || got[1] != (domain.Progress{Stage: "scoring", Done: 2, Total: 3}) {
		t.Fatalf("progress = %+v", got)
	}
	if a.RiskScore != 72 || a.Grade != domain.SeverityHigh || a.LossAmount != 1235 || a.LossMin != 100 || a.LossMax != 2000 {
		t.Fatalf("analysis = %+v", a)
	}
	if a.Deposit != nil || a.Flow != nil || a.Warnings != nil || a.ModelVersion != "m" || a.PolicyVersion != "p" {
		t.Fatalf("nullable = %+v", a)
	}
	c := a.Clauses[0]
	if len(a.Clauses) != 1 || c.LMax != 1235 || c.ExpectedLoss != 10 || c.LawReference != nil || c.Severity != domain.SeverityMedium || c.Confidence != 0.9 || c.SafeText != "s" {
		t.Fatalf("clause = %+v", c)
	}
}

func TestAnalyzeDepositAndFlow(t *testing.T) {
	a, err := analyze(t, ndjson(`{"type":"result","data":{"lossRange":[0,0],"facts":{"deposit":5000.0},"flow":{"k":[1]},"warnings":["w"],"clauses":[]}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Deposit == nil || *a.Deposit != 5000 || string(a.Flow) != `{"k":[1]}` || len(a.Warnings) != 1 {
		t.Fatalf("%+v", a)
	}
}

func TestRequestShape(t *testing.T) {
	for _, tc := range []struct{ token string }{{""}, {"secret"}} {
		t.Run("token="+tc.token, func(t *testing.T) {
			var gotToken, gotName, gotCT, gotPath string
			var gotBytes []byte
			var hasHeader bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_, hasHeader = r.Header["X-Internal-Token"]
				gotToken = r.Header.Get("X-Internal-Token")
				f, hdr, err := r.FormFile("file")
				if err != nil {
					t.Error(err)
					return
				}
				gotName = hdr.Filename
				gotCT = hdr.Header.Get("Content-Type")
				gotBytes, _ = io.ReadAll(f)
				fmt.Fprintln(w, resultLine)
			}))
			defer srv.Close()
			_, err := New(srv.URL, tc.token, 5*time.Second).Analyze(context.Background(), "계약서.pdf", []byte("%PDF-1.7 data"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if gotPath != "/v1/analyze/stream" || gotName != "계약서.pdf" || string(gotBytes) != "%PDF-1.7 data" || gotCT != "application/pdf" {
				t.Fatalf("path=%q name=%q bytes=%q ct=%q", gotPath, gotName, gotBytes, gotCT)
			}
			if hasHeader != (tc.token != "") || gotToken != tc.token {
				t.Fatalf("token header = %q (present=%v)", gotToken, hasHeader)
			}
		})
	}
}

func TestAnalyzeFailures(t *testing.T) {
	status := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			io.WriteString(w, body)
		}
	}
	tests := []struct {
		name string
		h    http.HandlerFunc
		msg  string
	}{
		{"422 detail", status(422, `{"detail":"스캔본이에요"}`), "스캔본이에요"},
		{"422 no detail", status(422, `{}`), msgUnreadble},
		{"422 non-string detail", status(422, `{"detail":[{"x":1}]}`), msgUnreadble},
		{"422 not json", status(422, `oops`), msgUnreadble},
		{"500", status(500, "boom"), msgGeneric},
		{"error readable", ndjson(`{"type":"error","detail":"읽을 수 없는 PDF","readable":true}`), "읽을 수 없는 PDF"},
		{"error not readable", ndjson(`{"type":"error","detail":"ValueError: x"}`), msgGeneric},
		{"no result", ndjson(`{"type":"progress","stage":"ocr","done":1,"total":2}`), msgNoResult},
		{"invalid json", ndjson(`{"type":"progress"`, resultLine), msgConnect},
		{"result without trailing newline ok? bad data", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"type":"result","data":"x"}`) }, msgConnect},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := analyze(t, tc.h, nil)
			wantMsg(t, err, tc.msg)
		})
	}
}

func TestStatusErrKeepsDebugInfo(t *testing.T) {
	_, err := analyze(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "boom")
	}, nil)
	ae := wantMsg(t, err, msgGeneric)
	if ae.Err == nil || !strings.Contains(ae.Err.Error(), "500") || !strings.Contains(ae.Err.Error(), "boom") {
		t.Fatalf("Err = %v", ae.Err)
	}
}

func TestOnProgressErrorStops(t *testing.T) {
	sentinel := errors.New("db down")
	calls := 0
	_, err := analyze(t, ndjson(
		`{"type":"progress","stage":"a","done":0,"total":1}`,
		`{"type":"progress","stage":"b","done":1,"total":1}`,
		resultLine,
	), func(domain.Progress) error { calls++; return sentinel })
	if err != sentinel {
		t.Fatalf("err = %v, want sentinel as-is", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close()
	_, err := New(url, "", time.Second).Analyze(context.Background(), "a.pdf", []byte("x"), nil)
	wantMsg(t, err, msgConnect)
}

func TestTimeout(t *testing.T) {
	// Both before the headers and mid-stream must give the timeout message.
	for name, h := range map[string]http.HandlerFunc{
		"before headers": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		},
		"mid stream": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, `{"type":"progress","stage":"a","done":0,"total":1}`)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			_, err := New(srv.URL, "", 100*time.Millisecond).Analyze(context.Background(), "a.pdf", []byte("x"), nil)
			wantMsg(t, err, msgTimeout)
		})
	}
}

func TestCallerCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := New(srv.URL, "", 5*time.Second).Analyze(ctx, "a.pdf", []byte("x"), nil)
	ae := wantMsg(t, err, "분석이 중단됐어요. 다시 올려 주세요")
	if !errors.Is(ae, context.Canceled) {
		t.Fatalf("Err = %v", ae.Err)
	}
}

func TestLargeResultLine(t *testing.T) {
	big := strings.Repeat("가", 600_000) // 1.8MB
	line := `{"type":"result","data":{"lossRange":[1,2],"clauses":[{"original":"` + big + `","severity":"low"}]}}`
	a, err := analyze(t, ndjson(line), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Clauses) != 1 || len(a.Clauses[0].Original) != len(big) {
		t.Fatal("큰 줄을 읽지 못함")
	}
}

// Which failures are worth another attempt: the AI server's trouble, yes; a problem with the input or the request, no.
func TestRetryable(t *testing.T) {
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
	}
	dropMidStream := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"type":"progress","stage":"ocr","done":1,"total":2}`)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // closes the connection without a clean end, like a crashed ai-service
	}
	tests := []struct {
		name string
		h    http.HandlerFunc
		want bool
	}{
		{"500", status(500), true},
		{"503", status(503), true},
		{"429", status(429), true},
		{"401 bad token", status(401), false},
		{"400", status(400), false},
		{"422 unreadable input", status(422), false},
		{"error event: internal failure", ndjson(`{"type":"error","detail":"ValueError: x"}`), true},
		{"error event: readable input problem", ndjson(`{"type":"error","detail":"읽을 수 없는 PDF","readable":true}`), false},
		{"connection drops mid stream", dropMidStream, true},
		{"no result", ndjson(`{"type":"progress","stage":"ocr","done":1,"total":2}`), false},
		{"invalid json", ndjson(`{"type":"progress"`, resultLine), false},
		{"undecodable result", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"type":"result","data":"x"}`) }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := analyze(t, tc.h, nil)
			var ae *svc.AnalysisError
			if !errors.As(err, &ae) {
				t.Fatalf("AnalysisError가 아님: %T %v", err, err)
			}
			if ae.Retryable != tc.want {
				t.Errorf("Retryable = %v, want %v (%v)", ae.Retryable, tc.want, ae)
			}
		})
	}

	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(nil)
		url := srv.URL
		srv.Close()
		_, err := New(url, "", time.Second).Analyze(context.Background(), "a.pdf", []byte("x"), nil)
		if ae := wantMsg(t, err, msgConnect); !ae.Retryable {
			t.Error("an unreachable AI server is worth retrying")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}))
		defer srv.Close()
		_, err := New(srv.URL, "", 100*time.Millisecond).Analyze(context.Background(), "a.pdf", []byte("x"), nil)
		if ae := wantMsg(t, err, msgTimeout); ae.Retryable {
			t.Error("a timeout already used up its time; do not retry")
		}
	})
	t.Run("caller cancelled", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// the server only notices a closed connection after the body is read, so do not wait for it forever
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		_, err := New(srv.URL, "", 5*time.Second).Analyze(ctx, "a.pdf", []byte("x"), nil)
		if ae := wantMsg(t, err, msgCanceled); ae.Retryable {
			t.Error("a cancellation is not a failure: the worker decides by the cancel cause")
		}
	})
}

// A connection attempt that timed out means the AI server could not be reached at all (it is restarting, or the route to
// it is not ready yet). That is not "the analysis took too long", and trying again may work.
func TestDialTimeoutIsRetryableButAReadTimeoutIsNot(t *testing.T) {
	dialTimesOut := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded} // "dial tcp 10.43.0.1:8000: i/o timeout"
		},
	}}
	_, err := New("http://ai-service:8000", "", 5*time.Second, WithHTTPClient(dialTimesOut)).Analyze(context.Background(), "a.pdf", []byte("x"), nil)
	ae := wantMsg(t, err, msgConnect)
	if !ae.Retryable {
		t.Error("a dial that timed out is a connection failure and must be retryable")
	}

	// the same i/o timeout while reading the response is a stalled analysis, not a connection failure
	readTimesOut := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
		},
	}}
	_, err = New("http://ai-service:8000", "", 5*time.Second, WithHTTPClient(readTimesOut)).Analyze(context.Background(), "a.pdf", []byte("x"), nil)
	if ae := wantMsg(t, err, msgTimeout); ae.Retryable {
		t.Error("a read timeout is not retryable")
	}
}
