#!/bin/sh
# exit 7  = JIT config file present with the expected content and mode
# exit 20+ = contract broken
[ -s /srg/jitconfig ] || exit 21
[ "$(cat /srg/jitconfig)" = "JIT-INTEGRATION" ] || exit 22
[ "$(stat -c %a /srg/jitconfig)" = "600" ] || exit 23
env | grep -q "JIT-INTEGRATION" && exit 24
if [ "${SRG_HOLD:-}" = "1" ]; then
    sleep 300
fi
exit 7
