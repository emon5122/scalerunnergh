# Changelog

## 1.0.0 (2026-11-12)


### ⚠ BREAKING CHANGES

* new config format (host, scale_sets); env-only mode and the polling orchestrator are removed.

### Features

* add CI workflow, E2E tests, README, and .gitignore ([fc6f3fc](https://github.com/emon5122/scalerunnergh/commit/fc6f3fc89c6c7897a59939d2fd0ea4f477487faa))
* initial implementation of autoscaling ephemeral GitHub Actions runner ([09b0f1c](https://github.com/emon5122/scalerunnergh/commit/09b0f1c8ea0b72bd27393f711142f59ab69a3ea6))
* rebuild on runner scale sets with docker and native backends ([0b844af](https://github.com/emon5122/scalerunnergh/commit/0b844af017e2032d7dc7ecbac0c2e366d73a4ffb))
* runner images, drain on shutdown, deploy files and release pipeline ([b340a82](https://github.com/emon5122/scalerunnergh/commit/b340a825edde5607fe3acdf1fe379567adbdf828))


### Bug Fixes

* fifth review round (running jobs survive an API outage) ([a025b0b](https://github.com/emon5122/scalerunnergh/commit/a025b0b44d405b7bd6976f4a5fd119f86fc5f67b))
* findings from the live spike against GitHub ([a2fc92f](https://github.com/emon5122/scalerunnergh/commit/a2fc92ffe314e4f823f7396bf78ca78125be7448))
* fourth review round (never drop a job unchecked) ([8b1144c](https://github.com/emon5122/scalerunnergh/commit/8b1144c01605b816cc461dbe5a6176dcb872bdc0))
* review findings in the scale set rewrite ([0cdf00f](https://github.com/emon5122/scalerunnergh/commit/0cdf00f01d0376d1c7ca5607b6745ede91016b0e))
* second review round (error handling and backoff) ([7eff730](https://github.com/emon5122/scalerunnergh/commit/7eff73018e2098b0387bebec2a4abf21ad786450))
* third review round (runner tarball layout, redelivery, bounds) ([46d66ba](https://github.com/emon5122/scalerunnergh/commit/46d66ba1c7dec0317e92f43b2dbaa9691d7bcae7))
