---
title: Run workers on Kubernetes
description: Install Casebox with its Helm chart and run workers next to it.
---

This guide installs Casebox on Kubernetes with the Helm chart in `deploy/helm/casebox`, with an external Postgres 16.

1. Create the Secrets the chart reads:

   ```bash
   kubectl create secret generic casebox-db --from-literal=password=<database password>
   kubectl create secret generic casebox-keys --from-literal=master-key="v1:$(openssl rand -base64 32)"
   ```

2. Install the chart:

   ```bash
   helm install casebox deploy/helm/casebox \
     --set database.host=<postgres host> \
     --set database.passwordSecret.name=casebox-db \
     --set keys.masterKeySecret.name=casebox-keys \
     --set oidc.authority=<issuer> --set oidc.clientId=<client id> \
     --set ingress.enabled=true --set ingress.host=casebox.example.com
   ```

   The chart runs the server and QueueBox, generates the tokens between them once, and keeps them across upgrades.
3. Add workers. A worker classifies interventions and drafts proposals with the analysis model, and reads your repositories with a read-only token:

   ```bash
   kubectl create secret generic casebox-worker \
     --from-literal=CASEBOX_WORKER_TOKEN=<worker token> \
     --from-literal=GITHUB_TOKEN=<read-only token> \
     --from-literal=AZURE_DEVOPS_TOKEN=<Code (Read) token, for Azure DevOps Server> \
     --from-literal=CASEBOX_ANALYSIS_PROVIDER=openai --from-literal=CASEBOX_ANALYSIS_MODEL=<model id> \
     --from-literal=OPENAI_API_KEY=<key>
   helm upgrade casebox deploy/helm/casebox --reuse-values \
     --set worker.enabled=true --set worker.envSecret=casebox-worker
   ```

The server's health answers on port 8081: the chart uses `/healthz/live` and `/healthz/ready` as probes, and `/metrics` for Prometheus. The ingress exposes port 8080 only.
