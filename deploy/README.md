# boox-bridge deploy runbook

Pipeline: Boox `.note` over WebDAV → parse + render → Claude vision HWR via `llm.jacomail.com` → Affine doc via local MCP. Single binary, systemd-managed, runs on LXC 145 (10.0.1.25) in the home lab.

## Build (from dev box)

```sh
go build -C ~/projects/boox-bridge -o /tmp/boox-bridge ./cmd/boox-bridge/
```

Cross-compile from any host: add `GOOS=linux GOARCH=amd64` before `go build`.

## Deploy (one-shot)

```sh
# push binary
ssh -i ~/.ssh/id_proxmox root@10.0.1.4 \
  "pct push 145 /tmp/boox-bridge /usr/local/bin/boox-bridge --mode 755"

# push systemd unit + env template
ssh -i ~/.ssh/id_proxmox root@10.0.1.4 \
  "pct push 145 ~/projects/boox-bridge/deploy/boox-bridge.service /etc/systemd/system/boox-bridge.service"

# inside LXC: create env from template, fill in secrets, then enable
ssh -i ~/.ssh/id_proxmox root@10.0.1.4 'pct exec 145 -- bash -lc "
  mkdir -p /etc/boox-bridge
  cat > /etc/boox-bridge/bridge.env <<EOF
LLM_GATEWAY_URL=https://llm.jacomail.com/anthropic
LLM_GATEWAY_TOKEN=<paste>
HWR_MODEL_DEFAULT=claude-sonnet-4-6
HWR_MODEL_ESCALATE=claude-opus-4-7
AFFINE_MCP_URL=http://10.0.1.21:3030/mcp
AFFINE_MCP_TOKEN=<paste>
AFFINE_WORKSPACE_ID=dc730ebd-42e8-4f0d-b3db-faf7b10f8c5a
AFFINE_PARENT_DOC_ID=S5GQRh5nis
BOOX_DATA_DIR=/var/lib/boox
OWNER_LABEL=claire
MAX_DAILY_LLM_USD=2.00
EOF
  chmod 600 /etc/boox-bridge/bridge.env
  systemctl daemon-reload
  systemctl enable --now boox-bridge
  systemctl status boox-bridge --no-pager | head -20
"'
```

## Smoke test

```sh
# drop a known good .note into the inbox via WebDAV path (mounted at /var/lib/boox/inbox/claire)
ssh -i ~/.ssh/id_proxmox root@10.0.1.4 \
  "pct push 145 ~/downloads/SkinRoutinesExport.note /var/lib/boox/inbox/claire/skin.note"
ssh -i ~/.ssh/id_proxmox root@10.0.1.4 \
  "pct exec 145 -- journalctl -u boox-bridge -f --no-pager"
```

Watch for one structured-log line per pipeline stage (`stage=dedup|parse|render|hwr|publish`). Successful completion ends with `done total_ms=…`. Failures move the file to `/var/lib/boox/dlq/` with a `<file>.err` companion.

## Debug helpers

- `boox-bridge` requires all listed env vars; it `exit 2`s on missing config.
- `journalctl -u boox-bridge -n 100 --no-pager` for recent activity.
- `cat /var/lib/boox/state/{seen,spend}.json` for skip-list and daily $-cap state.
- `ls /var/lib/boox/{inbox/claire,archive,dlq}/` to see the file lifecycle.
- `notedump <file>` (`go build ./cmd/notedump/`) for ad-hoc parser probing.
