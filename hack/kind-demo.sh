#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:-gang-demo}
K8S_VERSION=${K8S_VERSION:-v1.27.6}
IMAGE=${IMAGE:-ghcr.io/example/batch-scheduler:latest}
KIND_BIN=${KIND_BIN:-kind}
KUBECTL_BIN=${KUBECTL_BIN:-kubectl}
SKIP_BUILD=${SKIP_BUILD:-false}

workdir=$(cd "$(dirname "$0")/.." && pwd)
config_file=${workdir}/hack/${CLUSTER_NAME}-kind.yaml

cat >"${config_file}" <<EOF_CFG
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiVersion: kubeadm.k8s.io/v1beta3
    kubernetesVersion: v${K8S_VERSION}
EOF_CFG

echo "[kind] creating cluster '${CLUSTER_NAME}' using Kubernetes ${K8S_VERSION}" >&2
"${KIND_BIN}" create cluster --name "${CLUSTER_NAME}" --config "${config_file}"

echo "[kubectl] waiting for nodes to be ready" >&2
"${KUBECTL_BIN}" wait --for=condition=Ready node --all --timeout=180s

if [[ "${SKIP_BUILD}" != "true" ]]; then
  if command -v docker >/dev/null 2>&1; then
    echo "[docker] building scheduler image ${IMAGE}" >&2
    (cd "${workdir}" && docker build -t "${IMAGE}" .)
    echo "[kind] loading image into cluster" >&2
    "${KIND_BIN}" load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"
  else
    echo "[warn] docker not found; ensure ${IMAGE} is already accessible to the cluster" >&2
  fi
fi

echo "[kubectl] deploying scheduler" >&2
"${KUBECTL_BIN}" apply -f "${workdir}/deploy/batch-scheduler.yaml"
"${KUBECTL_BIN}" -n batch-scheduler-system set image deploy/batch-scheduler batch-scheduler="${IMAGE}" --record
"${KUBECTL_BIN}" -n batch-scheduler-system rollout status deploy/batch-scheduler --timeout=120s

echo "[kubectl] submitting gang demo job" >&2
"${KUBECTL_BIN}" apply -f "${workdir}/deploy/examples/gang-job.yaml"

cat <<'EOF_NOTE'
To watch the batch scheduling behavior:
  kubectl get pods -l batch.scheduling.k8s.io/gang=gang-demo -w
  kubectl -n batch-scheduler-system logs deploy/batch-scheduler -f --tail=50

Cleanup commands:
  kubectl delete -f deploy/examples/gang-job.yaml
  kubectl delete -f deploy/batch-scheduler.yaml
  kind delete cluster --name ${CLUSTER_NAME}
EOF_NOTE
