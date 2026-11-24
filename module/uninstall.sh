#!/system/bin/sh

MODDIR=${0%/*}
if "$MODDIR/bin/ddns" stop && [ ! -f /data/adb/ipv6-ddns/radio-restore.json ]; then
  rm -rf /data/adb/ipv6-ddns
fi
