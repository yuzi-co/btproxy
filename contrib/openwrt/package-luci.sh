#!/bin/sh
# Builds the LuCI page for btproxy, the same for every architecture: an .ipk for opkg (up to 24.10) and an .apk for
# apk (25.12 on).
#
#   contrib/openwrt/package-luci.sh <version> <output dir>
#
# Run it on Alpine 3.23 or later with GNU tar, like package.sh.
set -eu

version=$1
out=$2
here=$(cd "$(dirname "$0")" && pwd)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$out"

root=$work/root
install -D -m 0644 "$here/luci/htdocs/luci-static/resources/view/btproxy.js" \
	"$root/www/luci-static/resources/view/btproxy.js"
install -D -m 0644 "$here/luci/root/usr/share/luci/menu.d/luci-app-btproxy.json" \
	"$root/usr/share/luci/menu.d/luci-app-btproxy.json"
install -D -m 0644 "$here/luci/root/usr/share/rpcd/acl.d/luci-app-btproxy.json" \
	"$root/usr/share/rpcd/acl.d/luci-app-btproxy.json"

# LuCI caches its menu, and rpcd reads access rules when it starts or reloads.
cat > "$work/refresh" <<'SCRIPT'
#!/bin/sh
[ -n "$IPKG_INSTROOT" ] && exit 0
rm -f /tmp/luci-indexcache /tmp/luci-indexcache.*
rm -rf /tmp/luci-modulecache
/etc/init.d/rpcd reload >/dev/null 2>&1
exit 0
SCRIPT
chmod 0755 "$work/refresh"

desc='LuCI page for btproxy, the ESPHome-compatible Bluetooth proxy'
url=https://github.com/yuzi-co/btproxy

tarflags='--owner=0 --group=0 --numeric-owner --sort=name --mtime=@0'
ipk=$work/ipk
mkdir -p "$ipk/control"
tar $tarflags -C "$root" -czf "$ipk/data.tar.gz" .
size=$(wc -c < "$ipk/data.tar.gz")
cat > "$ipk/control/control" <<CONTROL
Package: luci-app-btproxy
Version: $version-1
Depends: luci-base, btproxy
Section: luci
Architecture: all
Installed-Size: $size
Maintainer: yuzi-co
License: MIT
Description: $desc
CONTROL
cp "$work/refresh" "$ipk/control/postinst"
cp "$work/refresh" "$ipk/control/postrm"
tar $tarflags -C "$ipk/control" -czf "$ipk/control.tar.gz" .
echo 2.0 > "$ipk/debian-binary"
tar $tarflags -C "$ipk" -czf "$out/luci-app-btproxy.ipk" ./debian-binary ./data.tar.gz ./control.tar.gz

apkversion=$(echo "$version" | tr - _)
apk mkpkg \
	--info "name:luci-app-btproxy" \
	--info "version:$apkversion-r1" \
	--info "description:$desc" \
	--info "arch:noarch" \
	--info "license:MIT" \
	--info "url:$url" \
	--info "depends:luci-base btproxy" \
	--script "post-install:$work/refresh" \
	--script "post-upgrade:$work/refresh" \
	--script "post-deinstall:$work/refresh" \
	--files "$root" \
	--output "$out/luci-app-btproxy.apk"
