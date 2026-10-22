#!/usr/bin/env bash
# End-to-end check of a deployed CleanSign: the Lima cluster, or the throwaway cluster the CI creates for a pull request.
#
#   scripts/e2e.sh [basic|crash|all]      (default: all)
#
#   basic  log in, upload a PDF, wait for the analysis, and check the result, the replicas and that the PDF was deleted
#   crash  kill the worker pod in the middle of an analysis and check that another attempt finishes it
#
# Variables: BASE_URL (default http://localhost), NS (namespace, default cleansign), PDF (default e2e/lease.pdf),
#            KUBECONFIG (as usual for kubectl), CRASH_TIMEOUT_S (default 360)
set -euo pipefail

cd "$(dirname "$0")/.."
BASE_URL="${BASE_URL:-http://localhost}"
NS="${NS:-cleansign}"
PDF="${PDF:-e2e/lease.pdf}"
CRASH_TIMEOUT_S="${CRASH_TIMEOUT_S:-360}"
API="$BASE_URL/api/v1"

k() { kubectl -n "$NS" "$@"; }
sql() { k exec postgres-0 -- psql -U postgres -d cleansign -tA -F' ' -c "$1"; }
log() { printf '▶ %s\n' "$*"; }

dump() {
  echo "--- pods"; k get pods -o wide || true
  echo "--- contracts"; sql "select left(id, 8), status, attempts, lease_until is not null as leased, next_run_at > now() as waiting from contract order by created_at desc limit 5" || true
  for app in server worker ai-service; do
    echo "--- logs: $app"; k logs -l "app=$app" --tail=25 --max-log-requests=5 --prefix 2>&1 | cut -c1-220 || true
  done
}

die() {
  printf '✖ %s\n' "$*" >&2
  dump >&2
  exit 1
}

# json_field NAME: the first string value of "NAME" read from a JSON document on stdin
json_field() { grep -oE "\"$1\":\"[^\"]*\"" | head -1 | cut -d'"' -f4; }

JAR="$(mktemp)"

# login signs up (or logs in) a fresh user. The login code is not mailed (no SMTP): it is in the server log.
login() {
  local email="e2e-$(date +%s)-$RANDOM@example.com" code="" i
  curl -fsS -X POST "$API/auth/email/code" -H 'content-type: application/json' -d "{\"email\":\"$email\"}" >/dev/null \
    || die "could not request a login code"
  for i in $(seq 1 20); do
    code="$(k logs -l app=server --tail=100 --max-log-requests=5 2>/dev/null | grep -F "to=$email" | grep -oE 'code=[0-9]{6}' | tail -1 | cut -d= -f2 || true)"
    [ -n "$code" ] && break
    sleep 1
  done
  [ -n "$code" ] || die "the login code for $email never showed up in the server log"
  curl -fsS -c "$JAR" -X POST "$API/auth/email/confirm" -H 'content-type: application/json' \
    -d "{\"email\":\"$email\",\"code\":\"$code\"}" >/dev/null || die "login failed"
  curl -fsS -b "$JAR" -X POST "$API/users/me/consents" -H 'content-type: application/json' \
    -d '{"terms":true,"privacy":true}' >/dev/null || die "could not save the consent"
}

upload() {
  [ -f "$PDF" ] || die "the test PDF $PDF does not exist"
  local id
  id="$(curl -fsS -b "$JAR" -X POST "$API/contracts" -F "file=@$PDF;type=application/pdf" | json_field id)"
  [ -n "$id" ] || die "the upload returned no id"
  echo "$id"
}

contract() { curl -fsS -b "$JAR" "$API/contracts/$1"; }

# wait_status ID WANT TIMEOUT_S: wait until the contract reaches WANT; fail early if it fails
wait_status() {
  local id="$1" want="$2" timeout="$3" start now body st
  start="$(date +%s)"
  while :; do
    body="$(contract "$id" || true)"
    st="$(printf '%s' "$body" | json_field status)"
    [ "$st" = "$want" ] && return 0
    [ "$st" = "failed" ] && die "contract $id failed: $(printf '%s' "$body" | json_field error)"
    now="$(date +%s)"
    [ $((now - start)) -ge "$timeout" ] && die "contract $id is still '$st' after ${timeout}s"
    sleep 2
  done
}

# wait_stage ID STAGE TIMEOUT_S: wait until the running analysis reports STAGE
wait_stage() {
  local id="$1" stage="$2" timeout="$3" start now
  start="$(date +%s)"
  until contract "$id" | grep -q "\"stage\":\"$stage\""; do
    now="$(date +%s)"
    [ $((now - start)) -ge "$timeout" ] && die "contract $id never reached stage '$stage'"
    sleep 1
  done
}

pdf_files() { sql "select count(*) from contract_file where contract_id = '$1'"; }

expect_no_pdf() {
  local id="$1" i
  for i in $(seq 1 10); do
    [ "$(pdf_files "$id")" = "0" ] && return 0
    sleep 1
  done
  die "the PDF of $id is still stored after the analysis ended"
}

basic() {
  log "waiting for the web page at $BASE_URL"
  local i
  for i in $(seq 1 60); do
    [ "$(curl -s -o /dev/null -m 5 -w '%{http_code}' "$BASE_URL/" || true)" = "200" ] && break
    [ "$i" = 60 ] && die "$BASE_URL never answered 200"
    sleep 2
  done

  log "the API refuses an anonymous request"
  [ "$(curl -s -o /dev/null -w '%{http_code}' "$API/users/me")" = "401" ] || die "GET /users/me without a login should be 401"

  log "replicas: server 2, worker 1, ai-service 1"
  [ "$(k get deploy server -o jsonpath='{.status.readyReplicas}')" = "2" ] || die "server should have 2 ready replicas"
  [ "$(k get deploy worker -o jsonpath='{.status.readyReplicas}')" = "1" ] || die "worker should have 1 ready replica"
  [ "$(k get deploy ai-service -o jsonpath='{.status.readyReplicas}')" = "1" ] || die "ai-service should have 1 ready replica"

  log "login, upload, analysis"
  login
  local id
  id="$(upload)"
  wait_status "$id" completed 180
  contract "$id" | grep -q '"riskItems":\[{' || die "the finished analysis has no risk items (the test PDF contains risky clauses)"

  log "the PDF is deleted once the analysis ended"
  expect_no_pdf "$id"
  log "basic: ok"
}

crash() {
  log "slowing ai-service down (MOCK_DELAY_S=2) so there is time to kill the worker mid-analysis"
  trap 'k set env deploy/ai-service MOCK_DELAY_S- >/dev/null 2>&1 || true; k rollout status deploy/ai-service --timeout=120s >/dev/null 2>&1 || true' EXIT
  k set env deploy/ai-service MOCK_DELAY_S=2 >/dev/null
  k rollout status deploy/ai-service --timeout=120s >/dev/null || die "ai-service did not restart"

  login
  local id
  id="$(upload)"
  log "waiting for the analysis to start working on clauses"
  wait_stage "$id" classifying 90

  log "killing the worker pod without any warning"
  k delete pod -l app=worker --grace-period=0 --force >/dev/null 2>&1 || die "could not delete the worker pod"
  [ "$(sql "select status from contract where id = '$id'")" = "pending" ] || die "the contract should still be pending right after the kill"

  log "waiting for another attempt to finish it (the lease has to run out first, about 2 minutes)"
  wait_status "$id" completed "$CRASH_TIMEOUT_S"
  local attempts
  attempts="$(sql "select attempts from contract where id = '$id'")"
  [ "$attempts" = "2" ] || die "expected the second attempt to finish it, but attempts = $attempts"
  expect_no_pdf "$id"
  log "crash: ok (finished on attempt $attempts)"
}

case "${1:-all}" in
  basic) basic ;;
  crash) crash ;;
  all) basic; crash ;;
  *) echo "usage: scripts/e2e.sh [basic|crash|all]" >&2; exit 2 ;;
esac
