#!/bin/sh
# Builds the signed OpenWrt feed from a release's packages, one directory per instruction set: all the btproxy
# packages share a name and are marked architecture independent, so they cannot share an index.
#
#   contrib/openwrt/feed.sh opkg <version> <dir with the packages> <output dir> <usign secret key>
#   contrib/openwrt/feed.sh apk  <version> <dir with the packages> <output dir> <EC private key>
#
# Run the opkg form on an OpenWrt root file system, which has usign. Run the apk form on Alpine 3.23 or later:
# "apk mkndx" is in Alpine's apk-tools, not in OpenWrt's.
#
# The output is <output dir>/opkg/<arch>/ or <output dir>/apk/<arch>/, with only the latest release in each.
set -eu

kind=$1
version=$2
packages=$3
out=$4
key=$5

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Adds one .ipk to the index in the current directory.
ipk_entry() {
	file=$1
	rm -rf "$work/control"
	mkdir -p "$work/control"
	tar -xzf "$file" -C "$work/control" ./control.tar.gz
	tar -xzf "$work/control/control.tar.gz" -C "$work/control" ./control
	{
		cat "$work/control/control"
		echo "Filename: $file"
		echo "Size: $(wc -c < "$file")"
		echo "SHA256sum: $(sha256sum "$file" | cut -d' ' -f1)"
		echo
	} >> Packages
}

found=
case $kind in
opkg)
	for package in "$packages"/btproxy-linux-*.ipk; do
		[ -e "$package" ] || continue
		found=1
		arch=${package##*/btproxy-linux-}
		arch=${arch%.ipk}
		dir=$out/opkg/$arch
		mkdir -p "$dir"
		cp "$package" "$dir/btproxy.ipk"
		if [ -e "$packages/btproxy-nobluez-linux-$arch.ipk" ]; then
			cp "$packages/btproxy-nobluez-linux-$arch.ipk" "$dir/btproxy-nobluez.ipk"
		fi
		cp "$packages/luci-app-btproxy.ipk" "$dir/"
		(
			cd "$dir"
			: > Packages
			ipk_entry btproxy.ipk
			if [ -e btproxy-nobluez.ipk ]; then
				ipk_entry btproxy-nobluez.ipk
			fi
			ipk_entry luci-app-btproxy.ipk
			gzip -9nc Packages > Packages.gz
		)
		usign -S -m "$dir/Packages" -s "$key" -x "$dir/Packages.sig"
	done
	;;
apk)
	apkversion=$(echo "$version" | tr - _) # as in package.sh
	for package in "$packages"/btproxy-linux-*.apk; do
		[ -e "$package" ] || continue
		found=1
		arch=${package##*/btproxy-linux-}
		arch=${arch%.apk}
		dir=$out/apk/$arch
		mkdir -p "$dir"
		# The index holds no file names: apk asks for <name>-<version>.apk.
		cp "$package" "$dir/btproxy-$apkversion-r1.apk"
		if [ -e "$packages/btproxy-nobluez-linux-$arch.apk" ]; then
			cp "$packages/btproxy-nobluez-linux-$arch.apk" "$dir/btproxy-nobluez-$apkversion-r1.apk"
		fi
		cp "$packages/luci-app-btproxy.apk" "$dir/luci-app-btproxy-$apkversion-r1.apk"
		# The packages themselves are unsigned. The signed index holds their hashes, and that is what the router checks.
		(cd "$dir" && apk mkndx --allow-untrusted --sign-key "$key" -o packages.adb ./*.apk)
	done
	;;
*)
	echo "unknown kind: $kind" >&2
	exit 2
	;;
esac

if [ -z "$found" ]; then
	echo "no btproxy packages in $packages" >&2
	exit 1
fi
