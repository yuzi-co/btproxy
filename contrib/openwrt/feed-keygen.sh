#!/bin/sh
# Generates the feed's signing keys, once, on the maintainer's machine. Needs Docker.
#
#   contrib/openwrt/feed-keygen.sh [--no-upload] <empty dir for the private keys>
#
# The private keys stay in that directory: copy it somewhere offline, because a lost or leaked key means every router
# has to install a new one by hand. The public keys go in contrib/openwrt/keys, to commit.
#
# Unless told --no-upload, it also sets up the repository, which needs gh signed in as an admin: it stores the private
# keys as secrets of the openwrt-feed environment, enables GitHub Pages, and lets only v* tags use either environment.
# Neither this script nor gh prints the keys.
set -eu

upload=1
if [ "$1" = --no-upload ]; then
	upload=
	shift
fi
dir=$1

here=$(cd "$(dirname "$0")" && pwd)
abspath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); } # pwd -W: the path Docker needs under Git Bash

mkdir -p "$dir"
if [ -n "$(ls -A "$dir")" ]; then
	echo "$dir is not empty, and this would replace its keys" >&2
	exit 1
fi
if [ -n "$upload" ] && [ -e "$here/keys/usign.pub" ]; then
	echo "$here/keys already has public keys: the feed has its keys" >&2
	exit 1
fi
mount=$(abspath "$dir")

MSYS_NO_PATHCONV=1 docker run --rm --entrypoint /bin/sh -v "$mount:/keys" openwrt/rootfs:x86_64-24.10.8 -c '
	umask 077
	usign -G -c "btproxy feed" -s /keys/usign.sec -p /keys/usign.pub'
# SEC1, which is what "openssl ecparam -genkey" writes: apk does not read the PKCS#8 form.
MSYS_NO_PATHCONV=1 docker run --rm -v "$mount:/keys" alpine:3.23 sh -c '
	umask 077
	apk add -q openssl
	openssl ecparam -name prime256v1 -genkey -noout -out /keys/apk.sec
	openssl ec -in /keys/apk.sec -pubout -out /keys/btproxy.pem 2>/dev/null'
chmod 600 "$dir/usign.sec" "$dir/apk.sec" 2>/dev/null || true
echo "Keys are in $dir. Copy them somewhere offline."

[ -n "$upload" ] || exit 0

mkdir -p "$here/keys"
cp "$dir/usign.pub" "$here/keys/usign.pub"
cp "$dir/btproxy.pem" "$here/keys/btproxy.pem"

repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)

# Only tags such as v1.0.0 can use these environments.
tags_only() {
	echo '{"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}' |
		gh api --method PUT "repos/$repo/environments/$1" --input - >/dev/null
	gh api --method POST "repos/$repo/environments/$1/deployment-branch-policies" \
		-f name='v*' -f type=tag >/dev/null
}

gh api --method POST "repos/$repo/pages" -f build_type=workflow >/dev/null
tags_only github-pages
tags_only openwrt-feed
gh secret set OPENWRT_USIGN_KEY --env openwrt-feed < "$dir/usign.sec"
gh secret set OPENWRT_APK_KEY --env openwrt-feed < "$dir/apk.sec"
echo "Repository set up. Commit contrib/openwrt/keys."
