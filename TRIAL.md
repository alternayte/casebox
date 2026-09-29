# Casebox trial on your work machine

This guide runs Casebox on your own Cursor CLI history. Casebox finds the corrections you keep making and proposes small changes to your repository that prevent them. You approve or reject each one, and apply it privately, so your team sees nothing until you choose to share.

Everything runs on your laptop. Only the classification and drafting calls leave it: they go to the model you choose in step 4.

## What you need

- Docker Desktop, running.
- The Cursor CLI, logged in. Run `cursor-agent status` to check.
- A git repository on GitHub in which you used the Cursor CLI. Its history is in `~/.cursor/projects/<folder>/agent-transcripts/`. A repository on Azure DevOps cannot be enrolled yet: `casebox init` refuses its remote name.
- An analysis model: your OpenAI key, or your Cursor login.
- A GitHub token that can read the repository (a fine-grained token with Contents: read). The worker reads your current `AGENTS.md`, rules and skills with it when it drafts a proposal.

## Keep the trial private

While you try Casebox alone:

- Connect no git host or tracker in `casebox init`: press Enter when it asks for GitHub or Jira. Pull request authors and reviewers are other people, and their data ends the solo view for good.
- Do not commit `.casebox/casebox.yml`. Add it to `.git/info/exclude`, your own ignore list: `echo /.casebox/ >> .git/info/exclude`.
- Apply proposals without `--commit` (step 7). Your teammates see no change.

Only your own history is read, and nothing is written to your git host.

## 1. Install the CLI

macOS or Linux:

```bash
curl -fsSL https://casebox-docs.pages.dev/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://casebox-docs.pages.dev/install.ps1 | iex
```

The script installs one binary into `~/.local/bin` (Windows: `%LOCALAPPDATA%\casebox\bin`) and checks it against the release checksum. If it tells you to add the directory to your PATH, do that and open a new terminal. If your company blocks github.com, see the comments at the top of the script.

## 2. Start the server

```bash
casebox up
```

This starts Postgres, QueueBox and the Casebox server in Docker, and prints the URL (`http://localhost:8080`) and the admin password. Keep the password. Run `casebox up` again at any time: it keeps the data and the password.

## 3. Enrol your repository

In the repository:

```bash
casebox init --prompt-mode redacted
echo /.casebox/ >> .git/info/exclude
```

1. A browser opens at a device page. Log in with the admin password, then click **Approve**.
2. Press Enter to skip GitHub and Jira.

`init` also installs capture hooks for the Cursor CLI, Claude Code and Codex, so new sessions are captured as you work. `casebox pause` stops capture, and `casebox uninstall` removes the hooks.

## 4. Start a worker

The worker classifies your corrections, groups them into patterns, and drafts proposals. Open a second terminal in the repository and choose one model.

**Your OpenAI key:**

```bash
export CASEBOX_WORKER_TOKEN=$(casebox token create --kind worker --name laptop)
export GITHUB_TOKEN=<read-only token>
export CASEBOX_ANALYSIS_PROVIDER=openai
export CASEBOX_ANALYSIS_MODEL=<a model your key allows>
export OPENAI_API_KEY=<your key>
casebox worker
```

If the key is for Azure OpenAI, also set `CASEBOX_ANALYSIS_BASE_URL=https://<resource>.openai.azure.com/openai/v1`, and set `CASEBOX_ANALYSIS_MODEL` to your deployment name.

**Your Cursor login (no key):**

```bash
export CASEBOX_WORKER_TOKEN=$(casebox token create --kind worker --name laptop)
export GITHUB_TOKEN=<read-only token>
export CASEBOX_ANALYSIS_PROVIDER=cursor-agent
casebox worker
```

The worker runs `cursor-agent` in read-only ask mode, in an empty folder (`~/.casebox/analysis-workspace`). Each call uses one request of your Cursor plan. The model is `auto`. To choose a model, set `CASEBOX_ANALYSIS_MODEL` to a name from `cursor-agent --list-models`. Free plans can use `auto` only.

Keep the worker running.

## 5. Import your history

Back in the first terminal:

```bash
casebox import --days 90 --wait 10m
```

The import reads your Cursor CLI, Claude Code and Codex sessions for this repository, removes secrets and marks identities on your laptop, and uploads them to your local server. It waits while the worker classifies, then prints the report and your top correction themes. Check its second line: it must count your Cursor CLI sessions. If it counts 0, tell me the folder name under `~/.cursor/projects`.

The report page (the URL `import` prints) says "Only your own sessions": while you are the only person, nothing is hidden.

## 6. Read the proposals

When 3 or more of your corrections share a cause, they form a pattern, and the worker drafts a proposal for it. This takes a few minutes after an import.

```bash
casebox proposals
casebox proposals show <id>
```

Each proposal shows the corrections behind it in your own words, why the change prevents them, and the change: an instruction bullet, a skill, an MCP server entry, or a code note with a prompt for your agent. The **Proposals** page in the web UI shows the same. At most three wait at once.

Approve or reject each one:

```bash
casebox proposals approve <id>
casebox proposals reject <id> --reason "why, in one line"
```

A rejected change does not come back for 90 days, and the next draft reads your reason.

## 7. Apply it privately

```bash
casebox apply <id>
```

The change is written as its own file (new bullets become the Cursor rule `.cursor/rules/casebox-<id>.mdc`) and listed in `.git/info/exclude`. `git status` stays clean, and your Cursor CLI reads the rule from its next session. A code note starts the Cursor CLI with its prompt instead; review that diff like any other change.

Keep working as usual. 30 days after an apply, `casebox proposals --all` shows whether that pattern's corrections fell.

## 8. Decide

Casebox is worth your team's time if these hold:

1. Your corrections are frequent enough to matter.
2. The patterns are right.
3. At least one applied proposal was followed by fewer corrections of its pattern.

When you want your team to have a change: `casebox apply <id> --commit`, then commit it and open a pull request as usual.

## Add your team

This part is not ready yet. Each teammate needs their own account, and Casebox makes accounts only through an OIDC sign-in, such as Entra ID. The local admin password is for one person, and teammates need a server they can reach, not your `localhost`. Tell me when you get here.

When a second person or account arrives, solo ends for good: the report shows a theme only when at least 3 people are behind it. Nobody sees a list or number per person.

## Stop and clean up

```bash
casebox uninstall       # remove the hooks and telemetry settings from Cursor, Claude Code and Codex
casebox down            # stop the server; the data stays
casebox down --volumes  # stop the server and delete its data
```

Then delete `~/.casebox`, the `casebox` binary, and any `.cursor/rules/casebox-*.mdc` files that `casebox apply` wrote.
