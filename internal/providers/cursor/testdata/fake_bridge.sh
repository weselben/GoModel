#!/bin/sh
# fake_bridge.sh — a minimal cursor-sdk-bridge stand-in for GoModel tests.
#
# Behavior is controlled by FAKE_BRIDGE_MODE:
#   ready    — write a token file, emit a valid ready line to stderr, sleep.
#              Sleeps long enough to outlive any test that wants to inspect
#              the (still running) process via Close.
#   fail     — write a diagnostic to stderr and exit 1 immediately.
#   hang     — sleep forever so the startup timeout can fire.
#
# The workspace dir is passed as the first positional argument (the test
# checks that GoModel used the placeholder it was given).
set -eu

mode=${FAKE_BRIDGE_MODE:-ready}
workspace=${1:-}
stderr_log=${FAKE_BRIDGE_STDERR_LOG:-}

if [ -n "$stderr_log" ]; then
    exec 2>>"$stderr_log"
fi

case "$mode" in
    fail)
        echo "fake bridge: configuration error: missing CURSOR_API_KEY" >&2
        exit 1
        ;;
    hang)
        # Sleep forever; let the parent timeout (and kill) us. exec so
        # the sleep replaces the shell — no grandchild can outlive a
        # killed bridge.
        exec sleep 3600
        ;;
    ready)
        # The token file path is supplied in FAKE_BRIDGE_TOKEN_FILE. We
        # write a fresh token there so the manager can read it back.
        token_file=${FAKE_BRIDGE_TOKEN_FILE:-}
        token=${FAKE_BRIDGE_TOKEN:-secret-test-token}
        if [ -z "$token_file" ]; then
            echo "fake bridge: FAKE_BRIDGE_TOKEN_FILE not set" >&2
            exit 2
        fi
        printf '%s\n' "$token" >"$token_file"
        chmod 0600 "$token_file"
        cat >&2 <<EOF
cursor-sdk-bridge ready {"schemaVersion":1,"serverVersion":"fake-1.0.0","pid":${$:-0},"transport":"tcp","protocol":"connect","host":"127.0.0.1","port":49152,"url":"http://127.0.0.1:49152","authTokenFile":"$token_file","workspaceRef":"$workspace","stateRoot":"/tmp/fake-state"}
EOF
        # Stay alive until SIGTERM/SIGKILL. exec so no grandchild can
        # outlive a killed bridge. Tests assert no leaked processes.
        exec sleep 3600
        ;;
    *)
        echo "fake bridge: unknown mode $mode" >&2
        exit 2
        ;;
esac
