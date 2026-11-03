#!/usr/bin/env bash
# Build the service images and load them into the cluster (Lima VM + k3s). No registry is involved.
#
#   scripts/images.sh [service...]   e.g. scripts/images.sh server ai-service   (default: all)
#
# Variables: TAG (default: local), VM (Lima instance, default: k3s-1), SERVER_URL (web build arg)
set -euo pipefail

cd "$(dirname "$0")/../.."   # repo root: build contexts are ./server ./ai-service ./web
TAG="${TAG:-local}"
VM="${VM:-k3s-1}"
SERVER_URL="${SERVER_URL:-http://server:4000}"   # rewrites in web are fixed at build time, so use the in-cluster name

services=("$@")
[ ${#services[@]} -gt 0 ] || services=(server ai-service web)

for svc in "${services[@]}"; do
  image="cleansign/${svc}:${TAG}"
  echo "▶ build ${image}"
  if [ "$svc" = web ]; then
    docker build --build-arg "SERVER_URL=${SERVER_URL}" -t "$image" "./${svc}"
  else
    docker build -t "$image" "./${svc}"
  fi
  echo "▶ load ${image} into ${VM}"
  docker save "$image" | limactl shell "$VM" sudo k3s ctr images import -
done
