#!/usr/bin/env bash
# Fake comms_send.sh for fixtures: records send attempts (so triage sending
# mail is a detectable failure); approval prints a fake conversation id.
echo "comms_send $*" >> "$ACTION_LOG"
[ "${1:-}" = "approval" ] && echo "conv_fake_approval"
exit 0
