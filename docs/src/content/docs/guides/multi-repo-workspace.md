---
title: Set up a multi-repo workspace
description: Group several repositories into one workspace with one environment, so cases can span them.
---

This guide puts several repositories into one workspace. A work item that changes two repositories then becomes one case, and the steering report groups them.

1. Run `casebox init --workspace checkout` in each repository, with the same workspace name. Each repository joins the workspace.
2. List every repository in each `casebox.yml` of the workspace:

   ```yaml
   workspace: checkout
   repos:
     - github.com/acme/checkout-api
     - github.com/acme/checkout-web
   ```

3. Write one environment recipe for all of them. When the repositories build from source together, name the files that link them under `links`, such as `go.work`, project references or an npm workspace file.

Cases come in three scopes:

| Scope | What the agent gets |
| --- | --- |
| Single | One sealed repository. |
| Split | The case's repository sealed; the other repositories of the work item at their merged state. |
| Multi | Every repository sealed side by side under one folder, each at its own base commit. Casebox needs `links` for this. |

Repositories that depend on each other only through published packages are not supported in 0.1.0, because such a case needs the historical package versions.
