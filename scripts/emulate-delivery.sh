#!/usr/bin/env bash
# Emulate the Azure DevOps service hook: read a pull request's current state,
# wrap it as a git.pullrequest.updated event, and POST it to a running receiver.
#
#   scripts/emulate-delivery.sh <pull-request-id> [receiver-url]
#
# Reads AZDO_ORG_URL, AZDO_PAT and WEBHOOK_SECRET from .env. Needs curl and jq.
# Without an AZDO_PAT it falls back to the az CLI's own login.
set -euo pipefail

pr_id=${1:?usage: emulate-delivery.sh <pull-request-id> [receiver-url]}
receiver=${2:-http://127.0.0.1:8081/webhook}
env_file="$(dirname "$0")/../.env"
[ -f "$env_file" ] || { echo "no .env found; copy .env.example to .env and fill it in" >&2; exit 1; }

setting() { grep -E "^$1=" "$env_file" | head -1 | cut -d= -f2-; }
org_url=$(setting AZDO_ORG_URL)
pat=$(setting AZDO_PAT)
secret=$(setting WEBHOOK_SECRET)

if [ -n "$pat" ]; then
  # An invalid PAT gets a 203 with an HTML sign-in page rather than a 401.
  pr=$(curl -sS -u ":$pat" -w '\n%{http_code}' "${org_url%/}/_apis/git/pullRequests/$pr_id?api-version=7.1")
  status=${pr##*$'\n'}
  pr=${pr%$'\n'*}
  if [ "$status" != 200 ]; then
    echo "Azure DevOps answered HTTP $status; check AZDO_PAT and its scopes" >&2
    exit 1
  fi
else
  pr=$(az repos pr show --org "$org_url" --id "$pr_id" -o json)
fi

payload=$(jq '{eventType: "git.pullrequest.updated", publisherId: "tfs", resourceVersion: "1.0", resource: .}' <<<"$pr")

echo "PR $pr_id: $(jq -r '.resource.title' <<<"$payload")" >&2
echo "reviewers: $(jq -r '[.resource.reviewers[].displayName] | join(", ")' <<<"$payload")" >&2

curl -sS -u "azdo:$secret" -H 'Content-Type: application/json' \
  --data-binary @- -w 'HTTP %{http_code}\n' "$receiver" <<<"$payload"
