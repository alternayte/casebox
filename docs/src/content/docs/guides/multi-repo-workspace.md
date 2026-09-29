---
title: Set up a multi-repo workspace
description: Group several repositories into one workspace, so their corrections form patterns together.
---

This guide puts several repositories into one workspace. The steering report and the patterns then group corrections across them, and a proposal can land in any of them.

1. Run `casebox init --workspace checkout` in each repository, with the same workspace name. Each repository joins the workspace.
2. List every repository in each `casebox.yml` of the workspace:

   ```yaml
   workspace: checkout
   repos:
     - github.com/acme/checkout-api
     - github.com/acme/checkout-web
   ```

A proposal targets the repository where most of its pattern's corrections happened. Run `casebox apply` in that repository.
