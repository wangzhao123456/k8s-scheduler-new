#!/usr/bin/env bash
set -euo pipefail

LOG_FILE=${LOG_FILE:-/tmp/dockerd.log}
SOCKET=${SOCKET:-/var/run/docker.sock}

if pgrep -x dockerd >/dev/null 2>&1; then
  echo "dockerd already running"
  exit 0
fi

echo "Starting dockerd with vfs storage and no iptables/bridge (suitable for CI containers without privileged networking)..."
nohup dockerd --host="unix://${SOCKET}" --storage-driver=vfs --iptables=false --ip-forward=false --bridge=none \
  >"${LOG_FILE}" 2>&1 &
PID=$!

sleep 2
if ! pgrep -x dockerd >/dev/null 2>&1; then
  echo "dockerd failed to start; see ${LOG_FILE}" >&2
  exit 1
fi

echo "dockerd started (pid ${PID}); logs: ${LOG_FILE}" 
