#!/bin/sh
# After removing the deb or rpm package. The configuration and the state store
# stay, even on a purge: the state remembers the quotas already spent and the
# rotated tokens, and losing it by accident is costly.
set -e
command -v systemctl >/dev/null 2>&1 || exit 0
systemctl daemon-reload || true
