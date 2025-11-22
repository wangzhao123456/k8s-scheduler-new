# Batch Scheduler for Kubernetes

This repository contains a minimal, production-focused gang-style scheduler built with the Kubernetes 1.27 client-go stack. Pods that share the same gang label are scheduled together (all or nothing) once the group reaches the configured minimum size.

## Features

- Watches pods that specify `schedulerName=batch-scheduler`.
- Gang label (`batch.scheduling.k8s.io/gang`) groups pods; default gang size is the full group or a configurable `min-available` annotation (integer or percentage).
- Validates node readiness and available CPU/memory before binding a gang atomically.
- Uses shared informers and a rate-limited workqueue for production-friendly performance.

## Building the scheduler image

```bash
# Build the binary
GOOS=linux GOARCH=amd64 go build -o bin/batch-scheduler ./cmd/batch-scheduler

# Build and push an image (example uses GHCR; replace with your registry)
REGISTRY=ghcr.io/your-org
IMAGE_TAG=${REGISTRY}/batch-scheduler:$(git rev-parse --short HEAD)
docker build -t ${IMAGE_TAG} .
docker push ${IMAGE_TAG}
```

Update `deploy/batch-scheduler.yaml` to point to your pushed image tag.

## Deploying on Kubernetes 1.27.6

### Quick automation with Kind

This repository includes a helper script that stands up a Kind cluster, loads a locally built scheduler image, deploys the scheduler, and submits the demo gang workload.

Prerequisites: `kubectl`, `kind`, and (optionally) `docker` for local image builds. All commands assume Kubernetes **1.27.6**.

```bash
# Install kind + kubectl if needed
curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.20.0/kind-linux-amd64 && chmod +x ./kind
sudo mv ./kind /usr/local/bin/
curl -LO "https://dl.k8s.io/release/v1.27.6/bin/linux/amd64/kubectl" && chmod +x kubectl
sudo mv ./kubectl /usr/local/bin/

# Create a demo cluster, build the scheduler image, and submit the example gang job
IMAGE=ghcr.io/your-org/batch-scheduler:demo ./hack/kind-demo.sh

# Watch pods bind together
kubectl get pods -l batch.scheduling.k8s.io/gang=gang-demo -w
```

If your environment lacks Docker, set `SKIP_BUILD=true` and ensure `IMAGE` points to a registry the cluster can pull from.

### Manual steps

1. **Create a cluster (Kind example)**
   ```bash
   cat <<'EOF' > kind.yaml
   kind: Cluster
   apiVersion: kind.x-k8s.io/v1alpha4
   nodes:
     - role: control-plane
     - role: worker
     - role: worker
   kubeadmConfigPatches:
     - |-
       kind: ClusterConfiguration
       apiVersion: kubeadm.k8s.io/v1beta3
       kubernetesVersion: v1.27.6
   EOF

   kind create cluster --name gang --config kind.yaml
   kubectl cluster-info
   ```

2. **Deploy the scheduler**
   ```bash
   # Update the image in deploy/batch-scheduler.yaml first
   kubectl apply -f deploy/batch-scheduler.yaml
   kubectl -n batch-scheduler-system set image deploy/batch-scheduler batch-scheduler=ghcr.io/your-org/batch-scheduler:demo

   # Confirm the deployment is healthy
   kubectl -n batch-scheduler-system get pods -w
   kubectl -n batch-scheduler-system logs deploy/batch-scheduler
   ```

3. **Submit a gang workload**
   ```bash
   kubectl apply -f deploy/examples/gang-job.yaml
   kubectl get pods -l batch.scheduling.k8s.io/gang=gang-demo -w
   ```

   Expected behavior: the three job pods stay Pending until all are present and capacity exists on the cluster; then they bind to nodes together.

4. **Validate gang scheduling**
   ```bash
   # See bindings
   kubectl get pods -l batch.scheduling.k8s.io/gang=gang-demo -o wide
   # Inspect scheduler logs for allocation decisions
   kubectl -n batch-scheduler-system logs deploy/batch-scheduler | tail -n 50
   ```

5. **Cleanup**
   ```bash
   kubectl delete -f deploy/examples/gang-job.yaml
   kubectl delete -f deploy/batch-scheduler.yaml
   kind delete cluster --name gang
   ```

## Configuration knobs

The container accepts flags:

- `--scheduler-name`: Scheduler name pods must reference (default `batch-scheduler`).
- `--gang-label`: Label key identifying gang membership (default `batch.scheduling.k8s.io/gang`).
- `--min-available-annotation`: Annotation that sets the minimum gang size; supports integer or percentage strings (default `batch.scheduling.k8s.io/min-available`).

## Example pod template snippet

```yaml
metadata:
  labels:
    batch.scheduling.k8s.io/gang: my-gang
  annotations:
    batch.scheduling.k8s.io/min-available: "3" # or "60%"
spec:
  schedulerName: batch-scheduler
```

## Notes for production use

- Run multiple replicas of the scheduler deployment if you want HA; Kubernetes will only use one leader because only one pod is needed to bind pods, and the code is idempotent on retries.
- Ensure RBAC is scoped appropriately for your cluster security posture.
- Consider building images with a distroless base and signing them for supply-chain security.
- Monitor logs and metrics; the scheduler currently logs key decisions to stdout.
### Running inside a minimal CI/container host

Some sandboxed environments (like this interactive container) do not provide systemd or allow iptables NAT rules, which prevents the standard Docker daemon from starting. You can still bring up a usable daemon for building/pushing images with a fallback configuration:

```bash
# install docker packages if missing
sudo apt-get update && sudo apt-get install -y docker.io

# start a local daemon without bridge/iptables side effects
./hack/start-dockerd.sh

# confirm the socket is live
docker ps
```

> ⚠️ Kind/minikube need working container networking (iptables NAT). If the daemon above is the only option, you won’t be able to create a local cluster here—run the deployment steps on a host with full Docker networking instead.

