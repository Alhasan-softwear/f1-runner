#!/bin/sh
set -e
/usr/sbin/sshd
exec dockerd-entrypoint.sh dockerd
