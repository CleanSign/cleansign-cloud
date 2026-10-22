// Package ai is the outbound adapter that calls the Python ai-service. It implements the Analyzer port.
package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	domain "github.com/CleanSign/cleansign/server/internal/domain/contract"
	svc "github.com/CleanSign/cleansign/server/internal/service/contract"
)

// Messages shown to the user as-is.
const (
	msgConnect   = "분석 서버에 연결하지 못했어요"
	msgTimeout   = "분석이 너무 오래 걸려 중단했어요. 잠시 후 다시 시도해 주세요"
	msgCanceled  = "분석이 중단됐어요. 다시 올려 주세요"
	msgUnreadble = "계약서를 읽을 수 없어요"
	msgGeneric   = "분석 중 문제가 생겼어요"
	msgNoResult  = "분석 결과를 받지 못했어요"
)

var _ svc.Analyzer = (*Client)(nil)

// Client is the ai-service HTTP client.
type Client struct {
	baseURL string
	token   string
	timeout time.Duration
	http    *http.Client
}

// Option changes the Client settings.
type Option func(*Client)

// WithHTTPClient swaps the http.Client, e.g. in tests.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// New creates a client. timeout is the time limit for the whole request (including reading the stream).
func New(baseURL, internalToken string, timeout time.Duration, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   internalToken,
		timeout: timeout,
		// The time limit is applied via context, so http.Client.Timeout is not used.
		http: &http.Client{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Analyze sends the PDF, reads the NDJSON stream while reporting progress, and returns the result.
// All failures are *svc.AnalysisError, except that an error returned by onProgress is returned as-is.
func (c *Client) Analyze(ctx context.Context, fileName string, pdf []byte, onProgress svc.ProgressFunc) (domain.Analysis, error) {
	var zero domain.Analysis

	reqCtx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	body, contentType, err := buildMultipart(fileName, pdf)
	if err != nil {
		return zero, &svc.AnalysisError{Message: msgGeneric, Err: err}
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.baseURL+"/v1/analyze/stream", body)
	if err != nil {
		return zero, &svc.AnalysisError{Message: msgGeneric, Err: err}
	}
	req.Header.Set("Content-Type", contentType)
	if c.token != "" {
		req.Header.Set("X-Internal-Token", c.token)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return zero, transportError(ctx, reqCtx, err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode == http.StatusUnprocessableEntity {
		// Unreadable input such as a scanned copy. detail can be shown to the user as-is.
		var b struct {
			Detail any `json:"detail"`
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		_ = json.Unmarshal(raw, &b)
		msg := msgUnreadble
		if s, ok := b.Detail.(string); ok && s != "" {
			msg = s
		}
		return zero, &svc.AnalysisError{Message: msg, Err: fmt.Errorf("ai-service 422: %s", snippet(raw))}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<10))
		// 5xx and 429 are the AI server's trouble and may pass; other 4xx (bad token, bad request) would fail again.
		retryable := res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests
		return zero, &svc.AnalysisError{Message: msgGeneric, Err: fmt.Errorf("ai-service %d: %s", res.StatusCode, snippet(raw)), Retryable: retryable}
	}

	// The result event carries the whole analysis, so one line can be hundreds of KB; use a Reader instead of a Scanner.
	r := bufio.NewReaderSize(res.Body, 64<<10)
	for {
		line, rerr := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var ev event
			if err := json.Unmarshal(line, &ev); err != nil {
				return zero, badPayload(ctx, reqCtx, fmt.Errorf("ndjson 파싱 실패: %w", err))
			}
			switch ev.Type {
			case "progress":
				if onProgress != nil {
					if err := onProgress(domain.Progress{Stage: ev.Stage, Done: ev.Done, Total: ev.Total}); err != nil {
						return zero, err
					}
				}
			case "result":
				a, err := decodeAnalysis(ev.Data)
				if err != nil {
					return zero, badPayload(ctx, reqCtx, err)
				}
				return a, nil
			case "error":
				msg := msgGeneric
				if ev.Readable && ev.Detail != "" {
					msg = ev.Detail
				}
				// readable means the input is the problem (shown to the user as is); otherwise it is an internal failure
				return zero, &svc.AnalysisError{Message: msg, Err: fmt.Errorf("ai-service error 이벤트: %s", ev.Detail), Retryable: !ev.Readable}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return zero, transportError(ctx, reqCtx, rerr)
		}
	}
	// Even if it looks like a normal end, a passed time limit means timeout is the cause.
	if reqCtx.Err() != nil {
		return zero, transportError(ctx, reqCtx, reqCtx.Err())
	}
	return zero, &svc.AnalysisError{Message: msgNoResult}
}

// buildMultipart builds a multipart body with a single file part in memory (PDFs are 10MB or less).
func buildMultipart(fileName string, pdf []byte) (*bytes.Reader, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(fileName)))
	h.Set("Content-Type", "application/pdf")
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(pdf); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return bytes.NewReader(buf.Bytes()), w.FormDataContentType(), nil
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "", "\n", "")

func escapeQuotes(s string) string { return quoteEscaper.Replace(s) }

// transportError classifies transport/read errors into user messages.
// parent is the caller's context and req the one that carries the time limit of the whole request.
//   - the caller cancelled: a cancellation
//   - the time limit of the whole request passed, or a read stalled: a timeout (the analysis already used its time)
//   - the connection could not be made, even if that attempt timed out: the AI server is unreachable (it may be restarting
//     or the network to it is not ready yet), which is worth retrying
//   - anything else: the connection broke
func transportError(parent, req context.Context, err error) error {
	if errors.Is(parent.Err(), context.Canceled) {
		return &svc.AnalysisError{Message: msgCanceled, Err: parent.Err()}
	}
	if errors.Is(req.Err(), context.DeadlineExceeded) {
		return &svc.AnalysisError{Message: msgTimeout, Err: err}
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return &svc.AnalysisError{Message: msgConnect, Err: err, Retryable: true}
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &svc.AnalysisError{Message: msgTimeout, Err: err}
	}
	return &svc.AnalysisError{Message: msgConnect, Err: err, Retryable: true}
}

// badPayload is for a response that arrived but cannot be understood. It looks like a connection failure to the user,
// but asking again would return the same thing.
func badPayload(parent, req context.Context, err error) error {
	e := transportError(parent, req, err)
	var ae *svc.AnalysisError
	if errors.As(e, &ae) {
		ae.Retryable = false
	}
	return e
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	return s
}

// ---- wire format ----

type event struct {
	Type     string          `json:"type"`
	Stage    string          `json:"stage"`
	Done     int             `json:"done"`
	Total    int             `json:"total"`
	Data     json.RawMessage `json:"data"`
	Detail   string          `json:"detail"`
	Readable bool            `json:"readable"`
}

type wireAnalysis struct {
	RiskScore  float64   `json:"riskScore"`
	Grade      string    `json:"grade"`
	LossAmount float64   `json:"lossAmount"`
	LossRange  []float64 `json:"lossRange"`
	Facts      struct {
		Deposit *float64 `json:"deposit"`
	} `json:"facts"`
	Clauses       []wireClause    `json:"clauses"`
	Flow          json.RawMessage `json:"flow"`
	Warnings      []string        `json:"warnings"`
	ModelVersion  string          `json:"modelVersion"`
	PolicyVersion string          `json:"policyVersion"`
}

type wireClause struct {
	Original     string   `json:"original"`
	ClauseType   string   `json:"clauseType"`
	Confidence   float64  `json:"confidence"`
	Severity     string   `json:"severity"`
	LMax         float64  `json:"lMax"`
	ExpectedLoss float64  `json:"expectedLoss"`
	Reason       string   `json:"reason"`
	LawReference []string `json:"lawReference"`
	SafeText     string   `json:"safeText"`
}

// round converts an amount to an integer in won, like TS's Math.round.
func round(v float64) int64 { return int64(math.Round(v)) }

func decodeAnalysis(raw json.RawMessage) (domain.Analysis, error) {
	var w wireAnalysis
	if len(raw) == 0 {
		return domain.Analysis{}, errors.New("result 이벤트에 data가 없음")
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return domain.Analysis{}, fmt.Errorf("분석 결과 해석 실패: %w", err)
	}
	a := domain.Analysis{
		RiskScore:     int32(math.Round(w.RiskScore)),
		Grade:         domain.Severity(w.Grade),
		LossAmount:    round(w.LossAmount),
		Warnings:      w.Warnings,
		ModelVersion:  w.ModelVersion,
		PolicyVersion: w.PolicyVersion,
	}
	if len(w.LossRange) >= 2 {
		a.LossMin, a.LossMax = round(w.LossRange[0]), round(w.LossRange[1])
	}
	if w.Facts.Deposit != nil {
		d := round(*w.Facts.Deposit)
		a.Deposit = &d
	}
	// JSON null remains as "null" in RawMessage, so convert it back to nil.
	if f := bytes.TrimSpace(w.Flow); len(f) > 0 && !bytes.Equal(f, []byte("null")) {
		a.Flow = append(json.RawMessage(nil), f...)
	}
	a.Clauses = make([]domain.AnalysisClause, len(w.Clauses))
	for i, c := range w.Clauses {
		a.Clauses[i] = domain.AnalysisClause{
			Original:     c.Original,
			ClauseType:   c.ClauseType,
			Confidence:   c.Confidence,
			Severity:     domain.Severity(c.Severity),
			LMax:         round(c.LMax),
			ExpectedLoss: round(c.ExpectedLoss),
			Reason:       c.Reason,
			LawReference: c.LawReference,
			SafeText:     c.SafeText,
		}
	}
	return a, nil
}
