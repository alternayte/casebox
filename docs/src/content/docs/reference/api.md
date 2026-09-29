---
title: API
description: Every route of the Casebox REST API, generated from the server's OpenAPI document.
---

This page lists every route of the API under `/api/v1`, grouped as the server groups them. The CLI, the GitHub Action and the web UI use only these routes. The full request and answer shapes are in the [OpenAPI document](/openapi.json).

- A person calls the API with a session cookie and the `X-CSRF-TOKEN` header, or with a CLI token from `casebox init` as `Authorization: Bearer cbx_cli_…`.
- Workers use `/worker/v1` with a worker token, and capture uses `/ingest/v1` with an ingest token.
- Every refusal is a problem answer with a `code` and a `type` that links its fix: see [error codes](/reference/errors/).

## Accounts

| Method | Path |
| --- | --- |
| GET | `/api/v1/accounts` |
| PUT | `/api/v1/accounts/{id}/role` |

## Auth

| Method | Path |
| --- | --- |
| GET | `/api/v1/auth/csrf` |
| POST | `/api/v1/auth/device/approve` |
| POST | `/api/v1/auth/device/code` |
| POST | `/api/v1/auth/device/token` |
| POST | `/api/v1/auth/local` |
| POST | `/api/v1/auth/logout` |
| GET | `/api/v1/auth/methods` |
| GET | `/api/v1/auth/oidc/login` |
| GET | `/api/v1/me` |

## Casebox.Server

| Method | Path |
| --- | --- |
| GET | `/worker/v1/patterns/{id}/evidence` |

## Ingest

| Method | Path |
| --- | --- |
| GET | `/ingest/v1/config` |
| POST | `/ingest/v1/sessions` |
| POST | `/v1/logs` |
| POST | `/v1/metrics` |

## Integrations

| Method | Path |
| --- | --- |
| GET | `/api/v1/integrations` |
| PUT | `/api/v1/integrations/azure-devops` |
| PUT | `/api/v1/integrations/github` |
| PUT | `/api/v1/integrations/jira` |
| DELETE | `/api/v1/integrations/{kind}` |
| GET | `/api/v1/repos/host` |

## Organisation

| Method | Path |
| --- | --- |
| GET | `/api/v1/org` |
| PUT | `/api/v1/org/settings` |

## Patterns

| Method | Path |
| --- | --- |
| GET | `/api/v1/patterns` |
| GET | `/api/v1/patterns/{id}` |
| POST | `/api/v1/patterns/{id}/acknowledgement` |
| POST | `/api/v1/patterns/{id}/dismissal` |

## Privacy

| Method | Path |
| --- | --- |
| POST | `/api/v1/privacy/erasures` |

## Proposals

| Method | Path |
| --- | --- |
| GET | `/api/v1/proposals` |
| GET | `/api/v1/proposals/{id}` |
| POST | `/api/v1/proposals/{id}/applied` |
| POST | `/api/v1/proposals/{id}/approval` |
| POST | `/api/v1/proposals/{id}/rejection` |

## Steering

| Method | Path |
| --- | --- |
| GET | `/api/v1/steering/agreement` |
| GET | `/api/v1/steering/interventions` |
| POST | `/api/v1/steering/refresh` |
| POST | `/api/v1/steering/relabel` |
| GET | `/api/v1/steering/report` |
| GET | `/api/v1/steering/status` |

## Tokens

| Method | Path |
| --- | --- |
| POST | `/api/v1/devices` |
| GET | `/api/v1/tokens` |
| POST | `/api/v1/tokens` |
| DELETE | `/api/v1/tokens/{id}` |

## Work

| Method | Path |
| --- | --- |
| PUT | `/api/v1/sessions/{id}/work-item` |
| GET | `/api/v1/work-items` |
| GET | `/api/v1/work-items/timeline` |

## Worker

| Method | Path |
| --- | --- |
| POST | `/worker/v1/attributions` |
| POST | `/worker/v1/jobs/lease` |
| POST | `/worker/v1/jobs/{id}/complete` |
| POST | `/worker/v1/jobs/{id}/fail` |
| POST | `/worker/v1/jobs/{id}/heartbeat` |
| GET | `/worker/v1/repos` |
| POST | `/worker/v1/sessions` |
| GET | `/worker/v1/steering/examples` |
| POST | `/worker/v1/steering/windows` |

## Workspaces

| Method | Path |
| --- | --- |
| GET | `/api/v1/workspaces` |
| POST | `/api/v1/workspaces` |
| GET | `/api/v1/workspaces/{name}` |
| POST | `/api/v1/workspaces/{name}/repos` |
| DELETE | `/api/v1/workspaces/{name}/repos/{repo}` |
