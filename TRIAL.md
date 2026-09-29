# Casebox trial on your work machine

This guide runs Casebox on your own Cursor CLI history. You get a report of how often you corrected the agent, what the corrections were about, and quotes of your own words. Then you decide whether to ask your team.

Everything runs on your laptop. Only the classification calls leave it: they go to the model you choose in step 4.

## What you need

- Docker Desktop, running.
- The Cursor CLI, logged in. Run `cursor-agent status` to check.
- A git repository in which you used the Cursor CLI. Its history is in `~/.cursor/projects/<folder>/agent-transcripts/`.
- An analysis model: your OpenAI key, or your Cursor login.

## 1. Install the CLI

macOS or Linux:

```bash
curl -fsSL https://casebox-docs.pages.dev/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://casebox-docs.pages.dev/install.ps1 | iex
```

The script installs one binary into `~/.local/bin` (Windows: `%LOCALAPPDATA%\casebox\bin`). It checks the download against the release checksum. If it tells you to add the directory to your PATH, do that and open a new terminal.

If your company blocks github.com, download the release from another machine and set `CASEBOX_DOWNLOAD_URL` and `CASEBOX_VERSION`. The comments at the top of the script explain these.

## 2. Start the server

```bash
casebox up
```

This starts Postgres, QueueBox and the Casebox server in Docker. It prints the URL (`http://localhost:8080`) and the admin password. Keep the password. Run `casebox up` again at any time: it keeps the data and the password.

## 3. Enrol your repository

In the repository:

```bash
casebox init --prompt-mode redacted
```

1. A browser opens at a device page. Log in with the admin password, then click **Approve**.
2. Press Enter to skip GitHub and Jira. The steering report does not need them.
3. Ignore the message that the environment is incomplete. The environment is for replaying cases, not for this trial.

`init` writes `.casebox/casebox.yml`. Do not commit it yet. It also installs capture hooks for the Cursor CLI, Claude Code and Codex, so that new sessions are captured as you work. `casebox pause` stops capture, and `casebox uninstall` removes the hooks.

## 4. Start a worker with an analysis model

The worker classifies each place where you stepped in: correction, direction, clarification or routine. Open a second terminal in the repository and choose one model.

**Your OpenAI key:**

```bash
export CASEBOX_WORKER_TOKEN=$(casebox token create --kind worker --name laptop)
export CASEBOX_ANALYSIS_PROVIDER=openai
export CASEBOX_ANALYSIS_MODEL=<a small model your key allows>
export OPENAI_API_KEY=<your key>
casebox worker
```

If the key is for Azure OpenAI, also set `CASEBOX_ANALYSIS_BASE_URL=https://<resource>.openai.azure.com/openai/v1`, and set `CASEBOX_ANALYSIS_MODEL` to your deployment name.

**Your Cursor login (no key):**

```bash
export CASEBOX_WORKER_TOKEN=$(casebox token create --kind worker --name laptop)
export CASEBOX_ANALYSIS_PROVIDER=cursor-agent
casebox worker
```

The worker runs `cursor-agent` in read-only ask mode, in an empty folder (`~/.casebox/analysis-workspace`). Each classification uses one request of your Cursor plan. The model is `auto`. To choose a model, set `CASEBOX_ANALYSIS_MODEL` to a name from `cursor-agent --list-models`. Free plans can use `auto` only.

Keep the worker running.

## 5. Import your history

Back in the first terminal:

```bash
casebox import --days 90 --wait 10m
```

The import reads your Cursor CLI, Claude Code and Codex sessions for this repository. It removes secrets and marks identities on your laptop, then uploads them to your local server. It waits while the worker classifies, then prints the report. Check the first line: it must count your Cursor CLI sessions. If it counts 0, the repository path and the folder name under `~/.cursor/projects` do not match. Tell me the folder name.

## 6. Read the report

Open the URL that `import` prints. You are the only person, so the report shows everything. A note at the top says "Only your own sessions".

Look at these:

- **Interventions and corrections:** How many times did you step in, and how many of those corrected the agent?
- **Correction themes:** Do the themes match what you remember fixing? Click a theme to see each correction.
- **Prevention mix:** Which share could a rule in your repository (AGENTS.md, Cursor rules) prevent?
- **Unclassified:** A high count means the model could not decide. Try another model.

To judge the classifier, run `casebox steering check` in the worker terminal, where the model settings are. It compares the model with a labelled set of 75 examples.

For more repositories, run `casebox init` and `casebox import` in each one. They all use the same server.

## 7. Decide

Casebox is worth the team's time only if all three are true:

1. Corrections are frequent enough to matter.
2. The themes are right.
3. At least one theme is something a rule could prevent.

If one of them is false, stop here and tell me which one.

## Add your team

This part is not ready yet. Each teammate needs their own account, and Casebox makes accounts only through an OIDC sign-in, such as Entra ID. The local admin password is for one person. `casebox up` does not set up OIDC yet, and teammates also need a server they can reach, not your `localhost`. Tell me when you get here, and I will make this step one command for you and `casebox join` for them.

When a second person or account arrives, solo ends for good. The report then shows a theme only when at least 3 people are behind it. Nobody sees a list or number per person.

## Stop and clean up

```bash
casebox uninstall       # remove the hooks and telemetry settings from Cursor, Claude Code and Codex
casebox down            # stop the server; the data stays
casebox down --volumes  # stop the server and delete its data
```

Then you can delete `~/.casebox` and the `casebox` binary.
