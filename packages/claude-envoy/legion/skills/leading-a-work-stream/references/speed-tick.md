# The speed tick: a reminder that survives restarts

A reminder inside the session (`sleep` in a background job) dies with the session's processes. A systemd user timer survives them and wakes the session with an Envoy message. Arm it once, from a shell on the machine the session runs on.

Replace `NAME` with a short name for your stream and `SESSION` with your full session id. The timer below fires at 01, 04, 07, 10, 13, 16, 19 and 22 UTC. Change `OnCalendar` for another rhythm.

```bash
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/speed-tick-NAME.service <<'EOF'
[Unit]
Description=Speed tick for NAME's lead

[Service]
Type=oneshot
ExecStart=<envoy-send-command> SESSION "SPEED TICK: run the speed loop in leading-a-work-stream now."
EOF
cat > ~/.config/systemd/user/speed-tick-NAME.timer <<'EOF'
[Unit]
Description=Speed tick for NAME's lead, every 3 hours

[Timer]
OnCalendar=*-*-* 01/3:00:00
Persistent=true

[Install]
WantedBy=timers.target
EOF
systemctl --user daemon-reload
systemctl --user enable --now speed-tick-NAME.timer
systemctl --user list-timers speed-tick-NAME.timer
```

Set `ExecStart` to the Envoy send command the deployment provides. A session that cannot install a user timer arms an equivalent wake-up and records how in its speed log.
