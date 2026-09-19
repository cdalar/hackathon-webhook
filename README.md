# hackathon-webhook

A small Go service that turns an Azure DevOps reviewer group into an AI code
reviewer. Add the **AI Assistant** group as a reviewer on a pull request and
Claude reviews the diff and posts its findings as a PR comment.

```
Azure DevOps PR ──(service hook: reviewers changed)──▶ POST /webhook
                                                          │ is "AI Assistant" a reviewer?
                                                          │ already reviewed this commit?
                                                          ▼
                              fetch PR diff ─▶ Claude review ─▶ post PR comment
```

## How it works

1. An Azure DevOps service hook delivers `git.pullrequest.updated` (and
   optionally `git.pullrequest.created`) events to `POST /webhook`.
2. The receiver ignores the event unless the PR is active and the configured
   reviewer ID is in the PR's reviewer list. The hook fires again on every
   later reviewer change, so each PR source commit is reviewed only once.
3. It acknowledges with `202` and, in the background, builds a unified diff of
   the PR's latest iteration from the Azure DevOps Git REST API, sends it to
   Claude (`claude-opus-5`), and posts the reply as a new comment thread.

Files that are binary, larger than 256 KB, or beyond the first 100 changed files
are not sent for review; the posted comment lists them under "Not reviewed".

The comment is authored by whoever owns the PAT — an Azure DevOps group cannot
author comments — so a dedicated service account gives the cleanest result.

## Configuration

| Variable | Required | Description |
|---|---|---|
| `AZDO_ORG_URL` | yes | Organization URL, e.g. `https://dev.azure.com/my-org`. API calls only ever go here. |
| `AZDO_PAT` | yes | Personal access token with **Code (Read)** and **Pull Request Threads (Read & write)** scopes. |
| `AI_REVIEWER_ID` | yes | Identity ID (GUID) of the reviewer group that triggers a review. |
| `ANTHROPIC_API_KEY` | yes* | Claude API key. *Not needed if you are logged in with `ant auth login`. |
| `WEBHOOK_SECRET` | recommended | If set, deliveries must send this as the HTTP basic-auth password. |
| `LISTEN_ADDR` | no | Listen address, default `:8080`. |

To find the reviewer group's ID:

```sh
az devops security group list --org https://dev.azure.com/my-org -p my-project \
  --query "graphGroups[?displayName=='AI Assistant'].originId" -o tsv
```

## Run

```sh
export AZDO_ORG_URL=https://dev.azure.com/my-org
export AZDO_PAT=...
export AI_REVIEWER_ID=...
export ANTHROPIC_API_KEY=...
export WEBHOOK_SECRET=...
go run .
```

`GET /healthz` returns `ok`. The service must be reachable from Azure DevOps over
HTTPS — deploy it behind TLS, or use a tunnel while developing.

## Docker

Every push to `main` publishes a multi-arch (amd64 + arm64) image to the GitHub
Container Registry, tagged `latest` and `sha-<commit>`; `v*` git tags also
publish `<version>` and `<major>.<minor>`.

```sh
docker run -d --name hackathon-webhook -p 8080:8080 \
  -e AZDO_ORG_URL -e AZDO_PAT -e AI_REVIEWER_ID \
  -e ANTHROPIC_API_KEY -e WEBHOOK_SECRET \
  ghcr.io/cdalar/hackathon-webhook:latest
```

For deployments, pin a `sha-<commit>` or version tag rather than `latest`. To
build locally instead: `docker build -t hackathon-webhook .`

`-e NAME` with no value passes the variable through from your shell, which keeps
secrets out of the command line and shell history; `--env-file .env` works too
(`.env` is git- and docker-ignored). The image is a static binary on distroless:
about 20 MB, runs as a non-root user, and has no shell. It listens on `:8080`
and serves plain HTTP, so put it behind something that terminates TLS (a cloud
load balancer, Caddy, a tunnel). Use `/healthz` for health checks. On `SIGTERM`
it stops accepting deliveries and waits for in-flight reviews to finish, so give
it a generous stop timeout (`docker stop -t 600`).

## Azure DevOps service hook

Project settings → Service hooks → **+** → Web Hooks:

- **Trigger:** Pull request updated
- **Repository:** the repo to watch
- **Change:** Reviewers changed
- **Reviewer includes group:** AI Assistant
- **URL:** `https://<your-host>/webhook`
- **Basic authentication password:** the value of `WEBHOOK_SECRET` (any username)

Reviewers added while *creating* a PR don't raise a "reviewers changed" event.
To cover that case, add a second hook with trigger **Pull request created** and
the same reviewer filter, pointing at the same URL.

## Test

```sh
go test -race ./...
```

The tests run against in-process fakes of Azure DevOps and the reviewer; they
make no network calls and need no credentials.
