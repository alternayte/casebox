---
title: Connect Jira Data Center
description: Link sessions and pull requests to Jira issues with a personal access token, without Jira admin rights.
---

This guide connects Jira Data Center so work items come from your Jira projects. Casebox polls Jira with your personal access token; it needs no Jira admin rights.

1. In Jira, open your profile, then **Personal Access Tokens**, and create a token.
2. In an enrolled repository, run:

   ```bash
   export CASEBOX_JIRA_TOKEN=<token>
   casebox init --jira-url https://jira.example.com --jira-project PAY
   ```

   Repeat `--jira-project` for each project. Without the variable, `init` asks for the token and hides what you type. The server checks the token against Jira before it stores it (`CBX072` when Jira refuses it).
3. Add the same URL and projects to `casebox.yml`:

   ```yaml
   work_items:
     jira: { url: https://jira.example.com, projects: [PAY] }
   ```

Casebox polls every 5 minutes with `project in (…) AND updated >= <cursor>` and requests only the configured host. It keeps the summary, description, type, labels, status and created and resolved dates. The assignee becomes a pseudonymous token.

A session links to an issue when its branch names the key (`PAY-123-retry`), when a pull request names it, or when you run `casebox link PAY-123`.
