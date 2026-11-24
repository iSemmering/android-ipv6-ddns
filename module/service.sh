#!/system/bin/sh

MODDIR=${0%/*}
umask 077
"$MODDIR/bin/ddns" start
