# GitHub App for ScaleRunnerGH

One App per organization it serves, plus one on a user account for personal
repos. Private Apps (`"public": false`) can be installed only on the account
that owns them, so each org owns its own App.

| Manifest | Owner | Permissions | Scale sets |
|---|---|---|---|
| [`org-manifest.json`](org-manifest.json) | an organization | Self-hosted runners: write (org), Actions: read, Metadata: read | org-level (`https://github.com/<org>`) |
| [`user-manifest.json`](user-manifest.json) | a user account | Administration: write, Actions: read, Metadata: read | repo-level (`https://github.com/<user>/<repo>`, `runner_group: "default"`) |

A user account has no runner groups, so personal repos get one repo-level scale
set each, and repo-level runners need *Administration: write*. No webhook and no
events are needed: the scale set API is a long poll.

## Create (manifest flow)

1. Replace `ORG` / `USER` in the manifest `name` (App names are global, for
   example `scalerunnergh-nexisltd`).
2. While logged in to GitHub as an owner, POST the manifest from any
   `github.com` tab (browser console, or a browser driver's evaluate):

   ```js
   const target = "https://github.com/organizations/ORG/settings/apps/new"; // user: https://github.com/settings/apps/new
   const manifest = { /* contents of the manifest file */ };
   const f = Object.assign(document.createElement("form"), { method: "post", action: target });
   f.appendChild(Object.assign(document.createElement("input"), { type: "hidden", name: "manifest", value: JSON.stringify(manifest) }));
   document.body.appendChild(f); f.submit();
   ```

3. Click **Create GitHub App for ORG** (GitHub may ask for sudo: passkey,
   GitHub Mobile or password). GitHub redirects to the repo page with
   `?code=...` in the address bar.
4. Within one hour, turn the code into the App id and key:

   ```bash
   deploy/github-app/convert.sh <code> <private dir>
   ```

   It prints `id`, `slug` and `client_id` and writes `private-key.pem` (0600);
   the key is never printed. Put the key in the secret store, then on each host
   as a 0600 file readable only by the service user.
5. Install the App on the same account (`<html_url>/installations/new`), on all
   repositories. The installation id is the number at the end of the
   installation's settings URL (`.../settings/installations/<id>`).

Config:

```json
{ "app": { "client_id": "Iv23...", "installation_id": 12345678, "private_key_file": "/run/secrets/app_key" } }
```
