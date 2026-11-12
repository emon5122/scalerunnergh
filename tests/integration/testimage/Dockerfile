# Stand-in for the runner image: it proves the orchestrator's contract (the
# JIT config arrives as a root-only file, never as an argument or env var).
FROM busybox:1.37.0
COPY entry.sh /entry.sh
ENTRYPOINT ["/bin/sh", "/entry.sh"]
