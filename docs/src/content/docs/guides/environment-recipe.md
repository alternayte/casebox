---
title: Write an environment recipe
description: Describe how to build and test your workspace once, so every case runs in the same environment.
---

This guide writes the environment recipe: the image, the install steps and the test commands that every case of a workspace runs with. Casebox builds the recipe once per workspace, and a person confirms it.

1. Draft it from what the repository has:

   ```bash
   casebox env draft
   ```

   The draft reads `devcontainer.json`, Dockerfiles, compose files and CI workflows. It prints an `environment:` block with the source of each field as a comment.
2. Edit the block into `casebox.yml`:

   ```yaml
   environment:
     image: golang:1.26
     lockfiles: [go.sum]
     install: ["go mod download"]
     test:
       - command: go test -json ./...
         results: go-test-json
         timeout: 10m
     services:
       postgres: { image: "postgres:16", env: { POSTGRES_PASSWORD: test } }
   ```

3. Build it and run the tests at HEAD:

   ```bash
   casebox env check
   ```

   The report says what passed, failed or timed out. A test that fetches from the network at run time shows here: sealed runs block the network, so fix it before you confirm.
4. Confirm it:

   ```bash
   casebox env confirm
   ```

Rules for a good recipe:

- Name a results format for each test command: `go-test-json`, `trx` or `junit`. Without one, Casebox counts only the exit code, so no case can prove that its change did the work.
- List the lockfiles. Their content keys the image cache, so a changed lockfile rebuilds the image and an unchanged one reuses it.
- Keep services in compose form. Each run gets its own services on a private network.
