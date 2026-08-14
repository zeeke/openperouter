#!/bin/bash
set -euo pipefail
set -x
CURRENT_PATH=$(dirname "$0")

source "${CURRENT_PATH}/../../common.sh"

export USE_HTTP=--use-http; export TLS_VERIFY=false; 
DEMO_MODE=true IMG_REPO="localhost:5000" make grout-deploy-operator-with-olm
export KUBECONFIG=$(pwd)/bin/kubeconfig



./bin/helm repo add metallb https://metallb.github.io/metallb

kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: metallb-system
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
EOF

# deploy metallb with frr-k8s as external backend
./bin/helm install metallb metallb/metallb \
  --namespace metallb-system \
  --set frrk8s.enabled=false \
  --set frrk8s.external=true \
  --set frrk8s.namespace=frr-k8s-system \
  --set speaker.ignoreExcludeLB=true \
  --set speaker.frr.enabled=false \
  --set frr-k8s.prometheus.serviceMonitor.enabled=false

IMG_TAG=main-grout IMG_REPO="localhost:5000" make grout-docker-build load-on-kind

wait_for_pods metallb-system app.kubernetes.io/name=metallb

${CONTAINER_ENGINE:-docker} image pull nginx:1.25
${KIND_BIN} --name pe-kind load docker-image nginx:1.25

apply_manifests_with_retries metallb.yaml openpe.yaml workload.yaml

