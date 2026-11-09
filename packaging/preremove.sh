#!/bin/sh
# Before removing the deb or rpm package, not on an upgrade.
# deb: $1 is "remove". rpm: $1 is 0.
set -e
command -v systemctl >/dev/null 2>&1 || exit 0
if [ "$1" = remove ] || [ "$1" = 0 ]; then
  systemctl disable --now tapyr.service || true
fi
