#!/bin/sh
# Builds a release's OpenWrt packages in Docker: btproxy for each architecture given, and the LuCI page.
#
#   contrib/openwrt/package-all.sh <version> <output dir> <arch>=<binary>...
#
# Paths are relative to the repository root, which is what the container sees. The packages come out owned by the
# caller rather than by root, which wrote them.
set -eu

if [ $# -lt 3 ]; then
	echo "usage: $0 <version> <output dir> <arch>=<binary>..." >&2
	exit 2
fi

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && { pwd -W 2>/dev/null || pwd; }) # pwd -W: the path Docker needs under Git Bash

# GNU tar, for the flags that make the archives reproducible; the arguments pass through as the inner shell's own.
"$here/alpine.sh" -v "$root:/src" -w /src -e "OWNER=$(id -u):$(id -g)" -- sh -c '
	set -e
	apk add -q tar
	version=$1
	out=$2
	shift 2
	for pair; do
		contrib/openwrt/package.sh "$version" "${pair%%=*}" "${pair#*=}" "$out"
	done
	contrib/openwrt/package-luci.sh "$version" "$out"
	chown -R "$OWNER" "$out"' sh "$@"
