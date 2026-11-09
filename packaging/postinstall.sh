#!/bin/sh
# After installing or upgrading the deb or rpm package. A first install does
# not start the service: without a configuration it would collect nothing.
# deb: $1 is "configure", $2 the previous version. rpm: $1 is 1 or 2.
set -e
command -v systemctl >/dev/null 2>&1 || exit 0
systemctl daemon-reload || true
if { [ "$1" = configure ] && [ -n "${2:-}" ]; } || [ "$1" = 2 ]; then
  # An upgrade: the state store is kept, the new version applies at once.
  systemctl try-restart tapyr.service || true
elif [ "$1" = configure ] || [ "$1" = 1 ]; then
  echo "Tapyr is installed. Write /etc/tapyr/config.yaml (see config.yaml.example there), then:"
  echo "  sudo systemctl enable --now tapyr"
fi
