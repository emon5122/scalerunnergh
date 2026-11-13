# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub Security Advisories: open the
repository's **Security** tab and click **Report a vulnerability**
(<https://github.com/emon5122/scalerunnergh/security/advisories/new>).
Do not open a public issue, pull request or discussion for a security problem.

Include the version (`scalerunnergh --version`), the backend (docker or native), the OS,
and the steps or config (with secrets removed) that reproduce it. You will get an answer
in the advisory thread; fixes ship in a patch release and the advisory is published after.

## Supported versions

| Version | Supported |
|---|---|
| Latest 1.x minor release | yes |
| Older 1.x minors, 0.x | no; upgrade to the latest 1.x |

## Safety model

ScaleRunnerGH runs other people's workflow code on your hosts, so it is built for one case:
**private repositories you trust**.

- Runners take jobs from private repositories only. The runner group must refuse public
  repositories (checked at start), and a guard re-checks every job and refuses public
  repositories and pull requests from forks. Every check fails closed.
- Never use it for public repositories or forks.
- With the docker backend and `dind: true`, the job container is privileged, so a job can
  reach root on its host. Use `docker.runtime: sysbox-runc` to avoid privileged containers.
- Native mode shares the host and the service account with the job.
- On Windows, `native.run_as` is defence in depth, not a security boundary against hostile
  jobs.
- Credentials (PAT or GitHub App key) are read from files only, never from the config or
  the environment.

Reports that need a public repository, a fork pull request or a hostile workflow in a
trusted repository to work are outside this model, but tell us if a documented check can
be bypassed.
