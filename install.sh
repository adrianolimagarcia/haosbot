#!/usr/bin/env bash
set -euo pipefail

# haosbot standalone installer
# Sets up ~/.haosbot/ (with ~/.nanobot compatibility), workspace templates,
# systemd service, and binary in ~/.local/bin/haosbot

INSTALL_DIR="${HOME}/.local/bin"
DATA_DIR="${HOME}/.haosbot"
LEGACY_DIR="${HOME}/.nanobot"
SYSTEMD_USER_DIR="${HOME}/.config/systemd/user"

echo "==> Installing haosbot..."

# 1. Ensure directories exist
mkdir -p "${INSTALL_DIR}"
mkdir -p "${DATA_DIR}/workspace"
mkdir -p "${DATA_DIR}/sessions"
mkdir -p "${DATA_DIR}/logs"
mkdir -p "${DATA_DIR}/media"
mkdir -p "${SYSTEMD_USER_DIR}"

# 1.1 If legacy ~/.nanobot/config.json exists and ~/.haosbot/config.json does not, copy it
if [ -f "${LEGACY_DIR}/config.json" ] && [ ! -f "${DATA_DIR}/config.json" ]; then
    cp "${LEGACY_DIR}/config.json" "${DATA_DIR}/config.json"
    echo "    Migrated config from ${LEGACY_DIR}/config.json to ${DATA_DIR}/config.json"
fi

# 2. Copy binary
if [ ! -f "./haosbot" ]; then
    echo "Error: ./haosbot binary not found in current directory." >&2
    exit 1
fi
cp ./haosbot "${INSTALL_DIR}/haosbot"
chmod +x "${INSTALL_DIR}/haosbot"
echo "    Installed binary to ${INSTALL_DIR}/haosbot"

# Also symlink nanobot -> haosbot for backward compatibility
ln -sf "${INSTALL_DIR}/haosbot" "${INSTALL_DIR}/nanobot"
echo "    Created compatibility symlink ${INSTALL_DIR}/nanobot -> haosbot"

# 3. Initialize paths & templates using haosbot itself
export PATH="${INSTALL_DIR}:${PATH}"
haosbot paths || true

# 4. Generate systemd unit file
SERVICE_FILE="${SYSTEMD_USER_DIR}/haosbot.service"
echo "==> Generating systemd user service: ${SERVICE_FILE}"
cat << 'EOF' > "${SERVICE_FILE}"
[Unit]
Description=haosbot AI Agent Gateway
After=network.target

[Service]
Type=simple
ExecStart=%h/.local/bin/haosbot gateway
Restart=always
RestartSec=3s
StandardOutput=append:%h/.haosbot/logs/gateway.log
StandardError=append:%h/.haosbot/logs/gateway.err.log

[Install]
WantedBy=default.target
EOF

echo "==> Done!"
echo "    haosbot installed successfully!"
echo "    Binary:  ${INSTALL_DIR}/haosbot"
echo "    Data:    ${DATA_DIR}/"
echo ""
echo "To manage the service via systemd:"
echo "    systemctl --user daemon-reload"
echo "    systemctl --user enable --now haosbot"
echo "    systemctl --user status haosbot"
