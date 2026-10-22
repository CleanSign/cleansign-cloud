"""A tiny stand-in for ai-service, used only by the deploy test in CI.

It speaks the part of the ai-service API that the server's worker uses (/healthz and POST /v1/analyze/stream, an NDJSON
stream of progress events and one result), so the queue, the retries and the worker crash scenario can be tested without
building the real service (onnxruntime, OCR) on every pull request. The real ai-service has its own tests and its image
is built by the Container Images workflow.

MOCK_DELAY_S adds a pause per clause, like the real mock provider, so a test can interrupt an analysis that is running.
"""
import json
import os
import signal
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CLAUSES = [
    ("선순위근저당", "high", 40_000_000, "임대인은 목적물에 근저당을 설정하지 않는다."),
    ("원상복구과도부담", "medium", 10_000_000, "임차인은 계약 종료 시 모든 시설물을 원상복구한다."),
    ("묵시적갱신배제", "medium", 8_000_000, "임차인은 갱신을 요구하지 않는다."),
    ("기타불이익", "low", 2_000_000, "임차인이 계약을 위반하면 위약금을 지급한다."),
    ("보증금반환제한", "high", 30_000_000, "보증금 반환은 신규 임차인이 입주한 뒤에 한다."),
]


def delay() -> float:
    try:
        return float(os.environ.get("MOCK_DELAY_S") or 0)
    except ValueError:
        return 0.0


def result() -> dict:
    clauses = [
        {
            "id": f"c{i}", "original": text, "clauseType": ctype, "confidence": 0.7, "severity": severity,
            "lMax": loss, "lMaxFormula": "stub", "expectedLoss": loss, "reason": "stub", "lawReference": None,
            "precedents": [], "safeText": "stub",
        }
        for i, (ctype, severity, loss, text) in enumerate(CLAUSES)
    ]
    total = sum(c["expectedLoss"] for c in clauses)
    return {
        "type": "result",
        "data": {
            "riskScore": 72, "grade": "high", "lossAmount": total, "lossRange": [total // 2, total * 2],
            "facts": {"deposit": 100_000_000, "monthlyRent": None, "leaseType": "전세"},
            "clauses": clauses, "flow": None, "warnings": None, "modelVersion": "stub", "policyVersion": "stub", "stats": {},
        },
    }


class Handler(BaseHTTPRequestHandler):
    # HTTP/1.0: the body ends when the connection closes, so no chunked encoding is needed for the stream
    protocol_version = "HTTP/1.0"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

    def do_GET(self):
        if self.path != "/healthz":
            self.send_error(404)
            return
        body = json.dumps({"status": "ok", "provider": "stub"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path != "/v1/analyze/stream":
            self.send_error(404)
            return
        self.rfile.read(int(self.headers.get("Content-Length") or 0))  # the PDF; not needed
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.end_headers()

        def emit(event: dict):
            self.wfile.write(json.dumps(event, ensure_ascii=False).encode() + b"\n")
            self.wfile.flush()

        emit({"type": "progress", "stage": "reading", "done": 0, "total": 0})
        emit({"type": "progress", "stage": "splitting", "done": 0, "total": 0})
        for i in range(len(CLAUSES)):
            time.sleep(delay())
            emit({"type": "progress", "stage": "classifying", "done": i, "total": len(CLAUSES)})
        time.sleep(delay())
        emit({"type": "progress", "stage": "scoring", "done": len(CLAUSES), "total": len(CLAUSES)})
        emit(result())


if __name__ == "__main__":
    # As PID 1 in a container Python ignores SIGTERM unless it handles it. Without this a replaced pod would keep answering
    # requests for its whole 30 second grace period, long after Kubernetes sent traffic elsewhere.
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    ThreadingHTTPServer(("0.0.0.0", 8000), Handler).serve_forever()
