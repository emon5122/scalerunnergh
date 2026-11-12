#!/usr/bin/env bash
# Turn the one-time code of the App manifest flow into the App's id, client id
# and private key. The key is written to <dir>/private-key.pem (mode 0600) and
# never printed. The code is valid for one hour and works once.
#
#   deploy/github-app/convert.sh <code> <dir>
set -euo pipefail
umask 077

if [ "$#" -ne 2 ]; then
    echo "usage: $0 <code> <dir>" >&2
    exit 2
fi
code="$1"
dir="$2"
case "${code}" in
    *[!A-Za-z0-9]* | "") echo "the code must be letters and digits only" >&2; exit 2 ;;
esac
command -v jq > /dev/null || { echo "jq is required" >&2; exit 1; }

mkdir -p "${dir}"
chmod 700 "${dir}"
resp="$(mktemp "${dir}/.conversion.XXXXXX")"
trap 'rm -f "${resp}"' EXIT

status="$(curl -sS -o "${resp}" -w '%{http_code}' -X POST \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    "https://api.github.com/app-manifests/${code}/conversions")"
if [ "${status}" != 201 ]; then
    echo "conversion failed: HTTP ${status}: $(jq -r '.message // "no message"' "${resp}" 2>/dev/null)" >&2
    exit 1
fi

jq -r '.pem' "${resp}" > "${dir}/private-key.pem"
jq '{id, slug, client_id, owner: .owner.login, html_url}' "${resp}" > "${dir}/app.json"
chmod 600 "${dir}/private-key.pem" "${dir}/app.json"
cat "${dir}/app.json"
echo "private key: ${dir}/private-key.pem (0600). Store it, then install the App:" >&2
echo "  $(jq -r '.html_url' "${resp}")/installations/new" >&2
