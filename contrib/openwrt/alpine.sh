#!/bin/sh
# Runs a command in the Alpine release the packaging scripts need, and is the one place that names it: "apk mkpkg"
# and "apk mkndx" are in apk-tools 3, which Alpine has from 3.23.
#
#   contrib/openwrt/alpine.sh [docker run options] -- <command> [arguments]
set -eu

image=alpine:3.23

# The image goes where the caller put "--", between the options and the command: each argument is moved to the end
# in turn, so the list keeps its order.
n=$#
placed=
while [ $n -gt 0 ]; do
	if [ -z "$placed" ] && [ "$1" = -- ]; then
		placed=1
		set -- "$@" "$image"
	else
		set -- "$@" "$1"
	fi
	shift
	n=$((n - 1))
done
if [ -z "$placed" ]; then
	echo "usage: $0 [docker run options] -- <command> [arguments]" >&2
	exit 2
fi

MSYS_NO_PATHCONV=1 exec docker run --rm "$@" # no path conversion under Git Bash
