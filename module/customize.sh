#!/system/bin/sh

ddns_version=$(sed -n 's/^version=//p' "$MODPATH/module.prop")
ddns_author=$(sed -n 's/^author=//p' "$MODPATH/module.prop")

ui_print "Android IPv6 DDNS"
ui_print "Version $ddns_version"
ui_print "Author $ddns_author"
ui_print "Email semmering@163.com"

case "$(getprop ro.product.cpu.abi)" in
  arm64-v8a) variant=arm64 ;;
  armeabi-v7a) variant=arm ;;
  x86_64) variant=amd64 ;;
  *) abort "当前设备架构不在安装包支持范围内" ;;
esac

mv "$MODPATH/bin/ddns-$variant" "$MODPATH/bin/ddns" || abort "缺少当前架构的程序"
rm -f "$MODPATH/bin/ddns-arm64" "$MODPATH/bin/ddns-arm" "$MODPATH/bin/ddns-amd64"
chmod 0755 "$MODPATH/bin/ddns" "$MODPATH/service.sh" "$MODPATH/uninstall.sh" || abort "无法设置模块文件权限"
ui_print "IPv6 DDNS 安装完成，重启后打开 WebUI 配置"
