# CleanSign Kubernetes deployment (local: Lima VM + k3s)
#
#   make secrets   Create overlays/local/secrets.env with random values (only if missing)
#   make up        Apply the local overlay to the cluster
#   make status    Show pods, services, volumes
#   make deploy S=server   Rebuild one service image (server|ai-service|web), load it into the VM, restart its pods (server also restarts the worker)
#   make e2e [T=basic|crash|all]   Run the end-to-end check against the cluster (needs BASE_URL if it is not http://localhost)
#   make forward   Open each service on its own local port (Ctrl+C stops all)
#                    web http://localhost:3000   server http://localhost:4000
#                    ai  http://localhost:8000   db     localhost:5433
#   make down      Remove everything in the cleansign namespace (the DB volume is deleted too)
#
# Variables: KUBECONFIG (default: the Lima k3s-1 VM), OVERLAY (default: local),
#            WEB_PORT SERVER_PORT AI_PORT DB_PORT (local ports for `make forward`)

KUBECONFIG ?= $(HOME)/.lima/k3s-1/copied-from-guest/kubeconfig.yaml
OVERLAY ?= local
WEB_PORT ?= 3000
SERVER_PORT ?= 4000
AI_PORT ?= 8000
DB_PORT ?= 5433
export KUBECONFIG

OVERLAY_DIR := k8s/overlays/$(OVERLAY)
NS := cleansign

.PHONY: secrets up deploy e2e status forward down

secrets:
	@[ -f $(OVERLAY_DIR)/secrets.env ] || { \
	  sed -e "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$$(openssl rand -hex 16)/" \
	      -e "s/^JWT_SECRET=.*/JWT_SECRET=$$(openssl rand -hex 32)/" \
	      -e "s/^INTERNAL_TOKEN=.*/INTERNAL_TOKEN=$$(openssl rand -hex 24)/" \
	      $(OVERLAY_DIR)/secrets.env.example > $(OVERLAY_DIR)/secrets.env; \
	  echo "▶ $(OVERLAY_DIR)/secrets.env 를 만들었어요"; }

up: secrets
	kubectl apply -k $(OVERLAY_DIR)

# The image tag stays "local", so applying the same YAML would change nothing. Restart the pods to pick up the new image.
# The worker runs the server image, so deploying server restarts the worker too.
DEPLOYMENTS = $(S) $(if $(filter server,$(S)),worker)

deploy:
	@[ -n "$(S)" ] || { echo "usage: make deploy S=server|ai-service|web"; exit 1; }
	scripts/images.sh $(S)
	for d in $(DEPLOYMENTS); do kubectl -n $(NS) rollout restart deploy/$$d; done
	for d in $(DEPLOYMENTS); do kubectl -n $(NS) rollout status deploy/$$d --timeout=180s; done

e2e:
	scripts/e2e.sh $(or $(T),all)

status:
	kubectl -n $(NS) get pods,svc,pvc -o wide

# kubectl port-forward: connects a local port to a pod through the API server (no Ingress involved). Runs until Ctrl+C.
forward:
	@trap 'kill $$(jobs -p) 2>/dev/null' INT TERM EXIT; \
	kubectl -n $(NS) port-forward svc/web $(WEB_PORT):3000 & \
	kubectl -n $(NS) port-forward svc/server $(SERVER_PORT):4000 & \
	kubectl -n $(NS) port-forward svc/ai-service $(AI_PORT):8000 & \
	kubectl -n $(NS) port-forward pod/postgres-0 $(DB_PORT):5432 & \
	wait

down:
	kubectl delete ns $(NS) --ignore-not-found
