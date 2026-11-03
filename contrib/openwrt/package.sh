#!/bin/sh
# Builds the OpenWrt packages for one architecture: an .ipk for opkg (up to 24.10) and an .apk for apk (25.12 on).
#
#   contrib/openwrt/package.sh <version> <amd64|arm64|armv7|mipsle> <binary> <output dir>
#
# The binary is static, so one package covers every OpenWrt target of an instruction set. Packages are therefore
# marked as architecture independent, and the post-install script checks that the binary runs on this router.
#
# Run it on Alpine 3.23 or later with GNU tar: "apk mkpkg" is in Alpine's apk-tools, not in OpenWrt's.
set -eu

version=$1
arch=$2
binary=$3
out=$4
here=$(cd "$(dirname "$0")" && pwd)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$out"

# Files
root=$work/root
install -D -m 0755 "$binary" "$root/usr/bin/btproxy"
install -D -m 0755 "$here/btproxy.init" "$root/etc/init.d/btproxy"
install -D -m 0600 "$here/btproxy.config" "$root/etc/config/btproxy"
conffiles=/etc/config/btproxy
# What sysupgrade keeps besides that. The files appear later, or never, which rules out listing them as conffiles.
install -d "$root/lib/upgrade/keep.d"
cat > "$root/lib/upgrade/keep.d/btproxy" <<'KEEP'
/etc/btproxy.json
/etc/btproxy.key
/etc/btproxy.uipass
/etc/bluetooth/keys/
KEEP

# Scripts
# A running binary holds its old copy on flash until it exits. With both copies there, a router with little flash
# runs out of space partway through unpacking and is left with a broken binary.
cat > "$work/preinst" <<'SCRIPT'
#!/bin/sh
[ -n "$IPKG_INSTROOT" ] && exit 0
[ -x /etc/init.d/btproxy ] && /etc/init.d/btproxy stop >/dev/null 2>&1
exit 0
SCRIPT
cat > "$work/postinst" <<SCRIPT
#!/bin/sh
[ -n "\$IPKG_INSTROOT" ] && exit 0
if ! /usr/bin/btproxy -h >/dev/null 2>&1; then
	echo "This btproxy package is for $arch and does not run on this router. Remove it and install the right one." >&2
	exit 1
fi
if [ "\$PKG_UPGRADE" = 1 ]; then
	/etc/init.d/btproxy enabled && /etc/init.d/btproxy start
	exit 0
fi
# A dbus that started before BlueZ was installed has not read its policy, so bluetoothd cannot get on the bus.
if ! pidof bluetoothd >/dev/null; then
	/etc/init.d/dbus restart >/dev/null 2>&1
	sleep 1
	/etc/init.d/bluetoothd restart >/dev/null 2>&1
fi
/etc/init.d/btproxy enable
/etc/init.d/btproxy start
exit 0
SCRIPT
cat > "$work/postupgrade" <<'SCRIPT'
#!/bin/sh
[ -n "$IPKG_INSTROOT" ] && exit 0
/etc/init.d/btproxy enabled && /etc/init.d/btproxy start
exit 0
SCRIPT
cat > "$work/prerm" <<'SCRIPT'
#!/bin/sh
[ -n "$IPKG_INSTROOT" ] && exit 0
[ "$PKG_UPGRADE" = 1 ] && exit 0
/etc/init.d/btproxy stop
/etc/init.d/btproxy disable
exit 0
SCRIPT
chmod 0755 "$work/preinst" "$work/postinst" "$work/postupgrade" "$work/prerm"

desc='ESPHome-compatible Bluetooth proxy for Home Assistant'
url=https://github.com/yuzi-co/btproxy

# .ipk: a gzipped tar of debian-binary, control.tar.gz and data.tar.gz, as OpenWrt's ipkg-build makes it.
tarflags='--owner=0 --group=0 --numeric-owner --sort=name --mtime=@0'
ipk=$work/ipk
mkdir -p "$ipk/control"
tar $tarflags -C "$root" -czf "$ipk/data.tar.gz" .
# The compressed size, as OpenWrt's own packages give it: the overlay compresses too, and opkg refuses a package
# that does not fit by this number.
size=$(wc -c < "$ipk/data.tar.gz")
cat > "$ipk/control/control" <<CONTROL
Package: btproxy
Version: $version-1
Depends: libc, kmod-bluetooth, bluez-daemon
Section: net
Architecture: all
Installed-Size: $size
Maintainer: yuzi-co
License: MIT
Description: $desc
CONTROL
printf '%s\n' "$conffiles" > "$ipk/control/conffiles"
cp "$work/preinst" "$work/postinst" "$work/prerm" "$ipk/control/"
tar $tarflags -C "$ipk/control" -czf "$ipk/control.tar.gz" .
echo 2.0 > "$ipk/debian-binary"
tar $tarflags -C "$ipk" -czf "$out/btproxy-linux-$arch.ipk" ./debian-binary ./data.tar.gz ./control.tar.gz

# .apk: OpenWrt reads the list of settings files from /lib/apk/packages.
apkroot=$work/apkroot
cp -a "$root" "$apkroot"
install -d "$apkroot/lib/apk/packages"
printf '%s\n' "$conffiles" > "$apkroot/lib/apk/packages/btproxy.conffiles"
apkversion=$(echo "$version" | tr - _) # apk's form of a pre-release: 1.0.0_rc1
apk mkpkg \
	--info "name:btproxy" \
	--info "version:$apkversion-r1" \
	--info "description:$desc" \
	--info "arch:noarch" \
	--info "license:MIT" \
	--info "url:$url" \
	--info "depends:libc kmod-bluetooth bluez-daemon" \
	--script "pre-install:$work/preinst" \
	--script "pre-upgrade:$work/preinst" \
	--script "post-install:$work/postinst" \
	--script "post-upgrade:$work/postupgrade" \
	--script "pre-deinstall:$work/prerm" \
	--files "$apkroot" \
	--output "$out/btproxy-linux-$arch.apk"
