# BuildKit GC for each CI Docker daemon

Docker's own BuildKit garbage collector keeps the build cache of a daemon under a size, with no code and no
schedule. Every daemon that runs CI jobs (the host daemon that scalerunnergh drives) should have it. The
`gc` block of scalerunnergh handles images; this file handles the build cache.

[`daemon.json`](daemon.json) holds only the setting:

```json
{ "builder": { "gc": { "enabled": true, "defaultKeepStorage": "10GB" } } }
```

## Rollout on one host (a person does this, with the host owner's yes)

The daemon restart stops every container on that daemon unless `live-restore` is on, so drain first.

```sh
# 0. Look first: what is there, and what the daemon config is today.
docker system df
docker info -f '{{.LiveRestoreEnabled}}'   # false: the restart in step 4 stops every container (stop on a shared prod daemon, e.g. rl9, and ask its owner)
sudo cat /etc/docker/daemon.json 2>/dev/null || echo '{}'

# 1. Merge (never overwrite: the file may hold other settings such as a data-root or log options).
sudo cp -a /etc/docker/daemon.json /etc/docker/daemon.json.bak.$(date +%F) 2>/dev/null || true
( sudo cat /etc/docker/daemon.json 2>/dev/null || echo '{}' ) \
  | jq -s '.[0] * .[1]' - daemon.json | sudo tee /etc/docker/daemon.json.new > /dev/null

# 2. Validate the merged file before it is used.
sudo dockerd --validate --config-file /etc/docker/daemon.json.new
sudo mv /etc/docker/daemon.json.new /etc/docker/daemon.json

# 3. Drain CI: touch the scalerunnergh kill switch and wait until /status shows no busy runners.
touch ./control/STOP
curl -s localhost:9090/status | jq '.scale_sets'

# 4. Restart the daemon, then lift the kill switch.
sudo systemctl restart docker
docker info -f '{{json .BuilderVersion}}'   # daemon is back
rm ./control/STOP

# 5. Check: the cache settles under the limit after the next builds.
docker system df
```

A rootless or separate daemon (for example a second `dockerd` with its own `--config-file`) has its own
daemon.json: merge into that file and restart that daemon's unit instead.

To roll back: restore the `.bak` file, validate, restart the daemon the same way.
