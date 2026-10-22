# CleanSign 쿠버네티스 배포 (local: Lima VM + k3s)

맥 안의 Lima VM(Ubuntu + k3s)에 web, server, worker, ai-service, postgres를 올리는 구성이다.
그림과 쿠버네티스 개념 설명은 [ARCHITECTURE.md](ARCHITECTURE.md)를 본다.
이 폴더는 앱 소스를 참조하지 않는다. 앱과의 접점은 이미지 이름(`cleansign/<서비스>`)과 환경변수 이름뿐이라, 나중에 통째로 다른 레포로 옮길 수 있다.

```
브라우저 ─ http://localhost ─▶ Traefik(Ingress) ─▶ web ─(/api/v1 rewrite)─▶ server ×2 ─▶ postgres (PVC)
                                                                                 │          ▲   (contract 테이블 = 분석 큐, PDF 임시 보관)
                                                                                 │          │ 작업을 가져감 (lease)
                                                                                 └─ 업로드 ─┘     worker ─▶ ai-service (mock)
```

server 는 업로드를 받아 DB 의 큐에 넣기만 하고, 분석은 별도 worker 가 가져가서 돌린다(아래 "분석 큐" 절).

## 폴더 구조

```
deploy/
├── Makefile                      # 자주 쓰는 명령 (아래 표)
├── scripts/images.sh             # 이미지 빌드 + VM 의 k3s 에 직접 import (레지스트리 없음)
└── k8s/
    ├── base/                     # 환경과 무관한 공통 매니페스트
    │   ├── namespace.yaml        #   cleansign
    │   ├── postgres.yaml         #   StatefulSet + PVC(2Gi)
    │   ├── ai-service.yaml       #   Deployment + Service
    │   ├── server.yaml           #   Deployment ×2 + Service (상태 없는 API)
    │   ├── worker.yaml           #   Deployment (같은 이미지, `server worker`) — 분석을 돌린다
    │   ├── web.yaml              #   Deployment + Service
    │   └── ingress.yaml          #   Traefik Ingress (/ → web)
    └── overlays/local/           # 이 맥의 Lima VM 용 (이미지 태그, Secret)
        ├── kustomization.yaml
        └── secrets.env.example   #   `make secrets` 가 secrets.env 를 만든다 (Git 제외)
```

## 처음 세팅

전제: Docker(Docker Desktop 등)가 실행 중이고, `limactl`, `kubectl`, `openssl`이 설치돼 있다.

```bash
# 1) Lima VM 만들기 (처음 한 번, Ubuntu 이미지를 받느라 몇 분 걸린다)
limactl create --name=k3s-1 --cpus=2 --memory=4 --disk=20 --tty=false template://k3s
limactl start k3s-1

# 2) 이미지 3개를 빌드해서 VM 에 넣기
cd deploy
scripts/images.sh

# 3) 배포 (secrets.env 가 없으면 무작위 값으로 만들어 준다)
make up
make status        # 파드가 전부 Running 인지 확인
```

`deploy/Makefile`은 기본으로 Lima VM의 kubeconfig(`~/.lima/k3s-1/copied-from-guest/kubeconfig.yaml`)를 쓴다. 다른 클러스터(Docker Desktop 등)가 기본 컨텍스트여도 이 Makefile은 영향을 받지 않는다. 다른 클러스터에 올리려면 `make up KUBECONFIG=~/.kube/config`처럼 지정한다.

## 접속

| 방법 | 주소 | 비고 |
|---|---|---|
| Ingress | http://localhost | 클러스터에 상시 열려 있다. 실서버와 같은 경로 |
| `make forward` | web `:3000`, server `:4000`, ai `:8000`, db `:5433` | 터미널을 켜 두는 동안만. 디버깅·DB 접속용. 포트는 `WEB_PORT=3100` 식으로 바꾼다 |

- 로그인 인증코드는 메일로 가지 않는다(SMTP 미설정). server 로그에 찍힌다: `kubectl -n cleansign logs deploy/server --tail=20 | grep -E '[0-9]{6}'` 또는 k9s에서 server 파드에 커서를 놓고 `l`.
- DB 접속: `psql -h localhost -p 5433 -U postgres -d cleansign` (비밀번호는 `k8s/overlays/local/secrets.env`의 `POSTGRES_PASSWORD`, `make forward` 실행 중일 때).

## 개발 → 배포 흐름

개발 중 빠른 확인은 저장소 루트의 `make run`(로컬 프로세스)으로 하고, 클러스터에서 확인할 때만 아래를 쓴다. 모든 명령은 `deploy/`에서 실행한다.

| 상황 | 명령 |
|---|---|
| 서버/AI/웹 코드를 고쳤다 | `make deploy S=server` (또는 `ai-service`, `web`) |
| DB 스키마를 바꿨다 | 마이그레이션 파일 추가 후 `make deploy S=server` (서버가 시작할 때 자동 적용) |
| 환경변수·리소스·replicas 를 바꿨다 | `k8s/` 의 YAML 수정 후 `make up` |
| 비밀값을 바꿨다 | `secrets.env` 수정 → `make up` → `kubectl -n cleansign rollout restart deploy/<이름>` |
| 상태·로그 확인 | `make status`, `kubectl -n cleansign logs deploy/server -f`, 또는 k9s |
| 전부 지우고 다시 | `make down` (DB 데이터까지 삭제) 후 `make up` |

이미지 태그가 항상 `local`이라 YAML 이 바뀌지 않으면 쿠버네티스는 파드를 교체하지 않는다. `make deploy`는 이미지를 넣은 뒤 `rollout restart`까지 해 준다.

## 배포 테스트 (CI)

PR 을 열면 `.github/workflows/deploy-test.yml`이 이 폴더의 매니페스트를 **임시 쿠버네티스 클러스터에 실제로 배포해서** 테스트한다(prizm-cloud 의 PR e2e 와 같은 목적이지만, 거기는 docker compose 이고 여기는 K8s 매니페스트 자체를 검증한다).

```
PR 열기 / 커밋 푸시 / Draft 해제   (server, ai-service, web, deploy 가 바뀔 때만, Draft 는 건너뜀)
  → 이미지 3개 빌드 (GitHub 호스트 러너, 무료)
  → k3d 로 임시 클러스터 생성 (Traefik, local-path 포함이라 Lima 와 같은 방식)
  → 이미지 import, `make up` 으로 deploy/k8s/overlays/local 적용, 파드 준비 대기
  → scripts/e2e.sh basic   로그인 → PDF 업로드 → 분석 완료 → PDF 삭제 확인
  → scripts/e2e.sh crash   분석 중 worker 파드를 강제 종료해도 다른 시도가 끝내는지 확인
  → 실패하면 파드 상태·로그를 artifact(deploy-test-diagnostics)로 올림, 클러스터는 항상 삭제
```

- 같은 확인을 내 클러스터에서 하려면 `make e2e` (`T=basic` 또는 `T=crash`로 나눠서도 된다). 기본은 `BASE_URL=http://localhost`.
- 테스트 PDF 는 `e2e/lease.pdf`(한글 조항 9개, mock 분류기가 위험으로 잡는 조항 포함). 크래시 시나리오는 ai-service 에 `MOCK_DELAY_S=2`를 잠시 걸어서 분석이 도중에 죽일 만큼 걸리게 하고, 끝나면 되돌린다.
- 크래시 시나리오는 lease 가 만료될 때까지(약 90초) 기다려야 해서 2분 반쯤 걸린다. 전체는 새 클러스터 기준 약 4분(이미지 빌드 제외).
- **내 맥에서 같은 절차를 미리 돌려 봤다**(새 k3d 클러스터를 만들어 배포하고 두 시나리오 실행). 다만 GitHub Actions 위에서 실제로 돌아가는 것은 첫 PR 에서 확인된다.
- **이 테스트가 잡은 실제 버그**: ai-service 를 막 교체한 직후 worker 의 연결 시도가 시간 초과(`dial ... i/o timeout`)로 끝나면 "분석이 오래 걸렸다"로 분류돼 재시도 없이 실패했다(`fix(infra)`). 새 클러스터에서 postgres 보다 server·worker 가 먼저 떠서 재시작을 거듭하던 것도 고쳤다(`fix(server)`, 재시작 13회 → 0회).

## 설계 메모

- **이미지 태그**는 `overlays/local/kustomization.yaml`의 `images:` 한 곳에서만 정한다. 레지스트리를 쓰게 되면 이름과 태그(커밋 SHA)만 바꾼다.
- **비밀값**은 Git에 올리지 않는다(`secrets.env`는 `.gitignore`). Secret은 overlay 의 `secretGenerator`가 만들고, overlay 에도 `namespace: cleansign`이 있어야 한다(없으면 `default`에 생겨서 파드가 "secret not found"로 멈춘다).
- **server**: 상태 없는 API. `replicas: 2`, `RollingUpdate`(surge 1, unavailable 0), `WORKER_EMBEDDED=false`(이 프로세스 안에서는 분석하지 않는다). 마이그레이션은 시작할 때 server 가 하고, advisory lock 이 있어서 두 파드가 동시에 떠도 충돌하지 않는다.
- **worker**: server 와 **같은 이미지**를 `args: ["worker"]`로 실행한다. HTTP API 는 없고 `:4001/healthz`(DB ping)만 연다. 마이그레이션은 하지 않는다. 동시 처리 1건(OCR 이 메모리를 많이 쓴다). 종료 시 진행 중인 분석을 최대 30초 기다리고 `terminationGracePeriodSeconds: 60`. `make deploy S=server`는 worker 도 함께 재시작한다.
- **ai-service**는 `LLM_PROVIDER=mock`이다. 이 VM에서 Ollama 모델은 돌릴 수 없다.
- **web**은 Next.js standalone이고 rewrites가 빌드 시점에 고정된다. `scripts/images.sh`가 `SERVER_URL=http://server:4000`을 빌드 인자로 넣는다. `HOSTNAME=0.0.0.0`을 지정하지 않으면 파드 IP 에만 바인딩되어 `localhost` 접속이 안 된다.

## 분석 큐

`contract` 테이블이 곧 큐다. 업로드는 계약(`pending`)과 PDF(`contract_file`)를 한 트랜잭션으로 저장하고 바로 응답한다.

- worker 는 `FOR UPDATE SKIP LOCKED`로 가장 오래된 작업을 가져가며 **lease(90초)** 를 건다. 분석하는 동안 30초마다 lease 를 연장(heartbeat)한다.
- worker 가 죽으면 lease 가 만료되고, 회수기(reaper, 30초마다)가 작업을 큐에 되돌린다. 다른 worker 가 이어받는다.
- 재시도: 최대 3회. 실패 뒤 30초, 두 번째 실패 뒤 2분 기다린다. 재시도할 가치가 있는 실패(AI 서버 연결 실패·5xx·스트림 도중 끊김)만 다시 한다. 읽을 수 없는 PDF, 타임아웃 등은 바로 실패 처리한다.
- 종료 신호(SIGTERM)를 받으면 새 작업을 더 안 가져가고, 진행 중인 분석을 최대 30초 기다린다. 그래도 안 끝나면 큐에 되돌리고 **시도 횟수에 세지 않는다**.
- PDF 는 분석이 끝나면(완료·최종 실패) 같은 트랜잭션에서 삭제한다. 24시간 넘게 처리되지 않은 작업은 가져가지 않고 실패 처리하면서 PDF 도 지운다. 사용자가 계약을 삭제하면 PDF 도 함께 지워진다.
- 같은 작업을 두 worker 가 끝내도(느린 쪽이 lease 를 잃은 뒤 끝나는 경우) 먼저 끝낸 결과만 저장된다(조항이 중복되지 않는다).

## 실험으로 확인한 것

| 실험 | 결과 |
|---|---|
| postgres 파드 삭제 | 새 파드가 같은 PVC에 붙어서 데이터 유지 |
| 분석 중 server 파드를 전부 삭제 | 분석에 영향 없음. 정상 완료(시도 1회) |
| **분석 중 worker 파드 강제 종료(크래시)** | 사용자에게는 계속 "분석 중". 약 91초에 회수기가 재큐, 124초에 새 worker 가 2번째 시도로 이어받아 141초에 완료. PDF 삭제됨. (큐 도입 전에는 분석이 유실되고 15분 뒤 "중단됐어요") |
| 분석 중 worker 파드 정상 종료(SIGTERM) | 유예 시간 안에 분석을 끝내고 완료(시도 1회) |
| 분석 중 ai-service 강제 종료(OOM 상황) | 3초 만에 재시도 가능 실패로 판단해 재큐, 30초 뒤 2번째 시도로 완료(총 57초) |
| server 교체 중 접속 (`Recreate`, 큐 도입 전) | 17개 요청 중 7개 실패(약 3.5초) |
| server 교체 중 접속 (`RollingUpdate`, 큐 도입 전) | 17개 중 1개 실패 (교체 끝나는 시점. 원인은 확인 못 함) |
| `kubectl rollout undo` | 세 리비전 모두 이미지가 `cleansign/server:local`로 같아서 실제 롤백이 안 됨 |
| ai-service 메모리 | 한도 768Mi에서 OCR 중 `OOMKilled`(exit 137) → 분석 실패. `request 512Mi / limit 2Gi`로 올려서 해결 (실측 157Mi → 완료 직후 374Mi) |

## 알려진 한계

1. **worker 가 죽으면 그 분석은 처음부터 다시 한다.** 이어서 하지 못하고 ai-service 를 다시 호출하므로 그만큼 시간과 자원이 든다. 재시도는 최대 3회다.
2. **롤백이 안 된다.** 태그가 `local`로 고정이라 이전 이미지가 사라진다. 커밋 SHA 태그를 붙이는 CI/CD와 레지스트리를 붙이면 해결된다.
3. **내 맥에서만 배포된다.** 이미지를 VM 에 직접 넣는 방식이라 다른 컴퓨터에서는 쓸 수 없다.
4. **postgres 가 클러스터 안에 있다.** VM 이 사라지면 데이터도 사라진다. 실사용 단계에서는 매니지드 DB 로 옮기고 `DATABASE_URL`만 바꾼다. 백업(`pg_dump` CronJob)도 아직 없다.
5. **업로드한 PDF 가 분석이 끝날 때까지 암호화 없이 DB(`contract_file.data`)에 있다.** 보관 시간은 짧지만(처리 시간, 최대 24시간) DB 와 그 백업이 유출되면 노출된다. 필요하면 앱 단계 암호화를 추가한다.
6. **큐의 시각 비교가 DB 시계와 worker 시계를 섞는다.** 작업 생성·실행 시각은 DB 의 `now()`, lease 와 재시도 시각은 worker 의 시계를 쓴다. 시계가 몇 초 어긋나면 작업 시작이 그만큼 늦어진다(통합 테스트에서 한 번 원인을 확인하지 못한 지연이 있었고, 실패 시 큐 상태를 출력하도록 해 두었다). 운영에서는 시간 동기화(NTP)를 유지한다.
7. **DB 하나가 큐와 데이터를 모두 맡는다.** 건수가 매우 많아지면 별도 큐나 오브젝트 스토리지(`FileStore` 포트 뒤에서 교체)를 검토한다.
8. 분석 점수·등급 정의는 앱 쪽에서 아직 틀려 있다(위험 조항이 있는데 grade 가 낮게 나올 수 있음). 배포와 무관한 앱 이슈다.

## 아직 안 한 것

- `overlays/prod` (새 컴퓨터용): 호스트명, `COOKIE_SECURE=true`, 이미지 출처만 다르게 둘 예정
- GitHub Actions 로 내 노트북에 **자동 배포**(배포 테스트는 위처럼 PR 에서 하지만, 배포 자체는 아직 수동이다), GHCR 이미지를 쓰는 배포, 도메인·DNS·TLS
- 멀티 노드(물리 분리), 모니터링·로그 수집(큐 길이, 재시도·실패율 알림), DB 백업

## 문제가 생겼을 때

| 증상 | 확인 |
|---|---|
| 파드가 `CreateContainerConfigError` | `kubectl -n cleansign describe pod <이름>`의 Events. "secret not found"면 Secret 이 다른 네임스페이스에 생겼는지 확인 |
| 파드가 `OOMKilled`(exit 137)로 재시작 | 메모리 `limits` 초과. `kubectl -n cleansign describe pod`의 Last State 확인 후 한도 조정 |
| `make deploy` 후에도 옛 동작 | 이미지가 VM 에 들어갔는지 `limactl shell k3s-1 sudo k3s ctr images ls -q \| grep cleansign` |
| kubectl 이 `127.0.0.1:<포트> connection refused` | 다른 클러스터(Docker Desktop 등)를 보고 있다. `export KUBECONFIG=~/.lima/k3s-1/copied-from-guest/kubeconfig.yaml` |
| 브라우저가 `https`로 바꿔서 안 열림 | `http://localhost`를 직접 입력 (로컬은 HTTPS 를 설정하지 않았다) |
