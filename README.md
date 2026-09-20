# hackathon-webhook

A small Go service that turns an Azure DevOps reviewer group into an AI
assistant for pull requests. Add the **AI Assistant** group as a reviewer and it
reads the PR's title, description, and changes, then:

- rewrites the title and description so they actually say what the PR does, and
- reviews the changed files, posting its comments on the lines they are about.

The AI is any server that speaks the OpenAI chat completions API — a local
llama.cpp, Ollama, vLLM, or LM Studio, or a hosted service — selected with one
URL. Nothing about the pull request leaves your network unless you point it
somewhere that does.

```
Azure DevOps PR ──(service hook: reviewers changed)──▶ POST /webhook
                                                          │ is "AI Assistant" a reviewer?
                                                          │ already enhanced?
                                                          ▼
               fetch title, description, diff ─▶ AI server ─▶ update PR title + description
                                                              (originals saved in a comment)
                                                            ─▶ comments on changed files + lines
```

## How it works

1. An Azure DevOps service hook delivers `git.pullrequest.updated` (and
   optionally `git.pullrequest.created`) events to `POST /webhook`.
2. The receiver ignores the event unless the PR is active and the configured
   reviewer ID is in the PR's reviewer list.
3. It acknowledges with `202` and, in the background, builds a unified diff of
   the PR's latest iteration from the Azure DevOps Git REST API and sends it to
   the AI server together with the current title and description.
4. In `update` mode (the default) it first posts the author's original title
   and description as a PR comment — Azure DevOps keeps no description history —
   and then replaces them with the AI's version. In `suggest` mode it leaves the
   PR untouched and posts the proposal as a comment instead.

5. Unless `REVIEW_COMMENTS=false`, it then asks the AI to review the changed
   files and posts each comment as a thread on the file and line it names, the
   same way a human reviewer's comments appear. The AI sees each file's diff
   with the new file's line numbers printed on every line, so it cites lines
   rather than computing them; a line it gets wrong anyway becomes a comment on
   the file as a whole. At most 10 comments per review, and a review with
   nothing to flag says so in a single comment.

6. With `REVIEW_SUGGESTIONS=true`, the AI may attach a **suggested change** to a
   finding: replacement code for the exact lines the comment is on, which the
   author can apply to the PR branch with one click. It is asked to do so only
   when the fix is a clean replacement of those lines; other findings stay
   plain comments. The receiver drops any suggestion it can't vouch for — a
   range that isn't wholly inside the diff or spans more than 20 lines, a
   no-op, or text that would break out of the suggestion block — and posts the
   comment without it. Nothing is ever applied automatically.

Informational comments — the saved originals, "no comments", "nothing to read" —
are created already **closed**, so they never count against a "comments must be
resolved" branch policy. Review findings are created **active**, because those
are for the author to resolve, and so is the proposal in `suggest` mode, since a
closed thread is collapsed out of sight.

The AI is told to keep whatever the diff can't show: motivation, work item
references like `AB#123`, links, rollout notes. Every description it writes ends
with a footer saying it was AI-enhanced; that footer is also how the receiver
knows not to rewrite the same description twice. The review runs once per source
commit, so a PR that gets new commits is reviewed again the next time the hook
fires for it.

Files that are binary, larger than 256 KB, or past the first 100 changed files
(or ~128 KB of diff) are not sent to the AI; the PR comment lists them. Output is
cut to Azure DevOps' limits (400-character title, 4000-character description).

Comments and edits are made as whoever owns the PAT — an Azure DevOps group
cannot act on its own — so a dedicated service account gives the cleanest result.

## Configuration

| Variable | Required | Description |
|---|---|---|
| `AZDO_ORG_URL` | yes | Organization URL, e.g. `https://dev.azure.com/my-org`. API calls only ever go here. |
| `AZDO_PAT` | yes | Personal access token with **Code (Read & write)** and **Pull Request Threads (Read & write)** scopes. `suggest` mode only needs Code (Read). |
| `AI_REVIEWER_ID` | yes | Identity ID (GUID) of the reviewer group that triggers an enhancement. |
| `AI_BASE_URL` | yes | Base URL of an OpenAI-compatible API, including `/v1`. See the examples below. |
| `AI_MODEL` | no | Model name to request. Default: the first model the server lists, which suits single-model local servers. |
| `AI_API_KEY` | no | Sent as a bearer token if set. Local servers usually need none. |
| `ENHANCE_MODE` | no | `update` (default) rewrites the PR; `suggest` only comments. |
| `REVIEW_COMMENTS` | no | `true` (default) posts review comments on the changed files; `false` turns the review off. |
| `REVIEW_SUGGESTIONS` | no | `true` lets review comments carry one-click suggested changes; default `false`. Needs `REVIEW_COMMENTS`. Reviews take noticeably longer with it on. |
| `WEBHOOK_SECRET` | recommended | If set, deliveries must send this as the HTTP basic-auth password. |
| `LISTEN_ADDR` | no | Listen address, default `:8080`. |

Typical `AI_BASE_URL` values:

| Server | `AI_BASE_URL` |
|---|---|
| llama.cpp `llama-server` | `http://my-llm-host:8080/v1` |
| Ollama | `http://my-llm-host:11434/v1` |
| vLLM | `http://my-llm-host:8000/v1` |
| LM Studio | `http://my-llm-host:1234/v1` |

Reasoning models work: the receiver sets no output-token cap, so a model can
think before it answers, and each run is bounded by a 10-minute timeout instead.
The server must support `response_format` JSON schemas or at least reply with a
JSON object; all four servers above do.

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
export AI_BASE_URL=http://my-llm-host:8080/v1
export WEBHOOK_SECRET=...
go run .
```

The receiver needs to reach two things: Azure DevOps (outbound HTTPS) and the AI
server. With a LAN-only AI server, run the receiver on that network — the same
machine as the model works well.

`GET /healthz` returns `ok`. The service must be reachable from Azure DevOps over
HTTPS — deploy it behind TLS, or use a tunnel while developing.

## Docker

Every push to `main` publishes a multi-arch (amd64 + arm64) image to the GitHub
Container Registry, tagged `latest` and `sha-<commit>`; `v*` git tags also
publish `<version>` and `<major>.<minor>`.

```sh
docker run -d --name hackathon-webhook -p 8080:8080 \
  -e AZDO_ORG_URL -e AZDO_PAT -e AI_REVIEWER_ID \
  -e AI_BASE_URL -e WEBHOOK_SECRET \
  ghcr.io/cdalar/hackathon-webhook:latest
```

For deployments, pin a `sha-<commit>` or version tag rather than `latest`. To
build locally instead: `docker build -t hackathon-webhook .`

`-e NAME` with no value passes the variable through from your shell, which keeps
secrets out of the command line and shell history. Or copy `.env.example` to
`.env`, fill it in, and use `--env-file .env` (`.env` is git- and
docker-ignored). The image is a static binary on distroless:
about 20 MB, runs as a non-root user, and has no shell. It listens on `:8080`
and serves plain HTTP, so put it behind something that terminates TLS (a cloud
load balancer, Caddy, a tunnel). Use `/healthz` for health checks. On `SIGTERM`
it stops accepting deliveries and waits for in-flight enhancements to finish, so give
it a generous stop timeout (`docker stop -t 600`).

## Try it without a service hook

`scripts/emulate-delivery.sh` fetches a pull request's current state and POSTs
it to a running receiver exactly as the service hook would, so you can test the
whole flow before exposing anything to the internet. It needs `curl`, `jq`, and
a filled-in `.env`.

```sh
cp .env.example .env    # then fill in .env
docker run -d --name hackathon-webhook -p 127.0.0.1:8081:8080 --env-file .env \
  ghcr.io/cdalar/hackathon-webhook:latest

scripts/emulate-delivery.sh 42      # 42 = pull request ID; expect "HTTP 202"
docker logs -f hackathon-webhook    # expect "PR 42: enhanced (update mode)"
                                    #    then "PR 42: reviewed, N comments"
```

The AI Assistant group must already be a reviewer on that PR. An enhanced
description is never rewritten again (delete its footer to force that), and a
running receiver reviews each commit once; restart the container to review the
same commit again.

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

The tests run against in-process fakes of Azure DevOps and the AI server; they
make no network calls and need no credentials. To try the prompts against a real
model:

```sh
AI_BASE_URL=http://my-llm-host:8080/v1 go test -run 'Live$' -v .
```
