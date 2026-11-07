#!/bin/sh
# Builds the signed feed with throwaway keys and uses it from a real OpenWrt root file system: a key it does not trust
# and tampered files must be refused, and a router that installed from a file must be able to move onto the feed.
#
#   test/openwrt_feed.sh <openwrt version> <dir with an older btproxy-linux-amd64 package> <dir with the new packages> <new version>
#
# The feed is served over HTTP from inside the container, as there is no certificate to serve HTTPS with. The new
# directory needs btproxy-linux-amd64 and luci-app-btproxy. Needs Docker and an x86_64 host.
set -eu

version=$1
abspath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); } # pwd -W: the path Docker needs under Git Bash
old=$(abspath "$2")
new=$(abspath "$3")
newversion=$4
src=$(abspath "$(dirname "$0")/..")

case $version in
24.*|23.*) kind=opkg ;;
*) kind=apk ;;
esac

scratch=$(mktemp -d)
work=$(abspath "$scratch")
name=btproxy-feed-test-$$
. "$(dirname "$0")/openwrt_lib.sh"
cleanup() {
	timed 30 docker stop -t 1 "$name" >/dev/null 2>&1 || true
	# The containers wrote these as root.
	timed 60 docker run --rm -v "$work:/work" alpine:3.23 sh -c 'rm -rf /work/* /work/.[!.]*' >/dev/null 2>&1 || true
	rm -rf "$scratch" 2>/dev/null || true
}
trap cleanup EXIT

# The feed, as the release workflow builds it.
if [ "$kind" = opkg ]; then
	MSYS_NO_PATHCONV=1 docker run --rm --entrypoint /bin/sh -v "$src:/src:ro" -v "$new:/pkg:ro" -v "$work:/work" \
		"openwrt/rootfs:x86_64-$version" -c "
			set -e
			usign -G -s /work/key.sec -p /work/key.pub
			/src/contrib/openwrt/feed.sh opkg $newversion /pkg /work/feed /work/key.sec"
else
	MSYS_NO_PATHCONV=1 docker run --rm -v "$src:/src:ro" -v "$new:/pkg:ro" -v "$work:/work" alpine:3.23 sh -c "
		set -e
		apk add -q openssl
		openssl ecparam -name prime256v1 -genkey -noout -out /work/key.sec
		openssl ec -in /work/key.sec -pubout -out /work/key.pub 2>/dev/null
		/src/contrib/openwrt/feed.sh apk $newversion /pkg /work/feed /work/key.sec"
fi

# A router, set up as in openwrt_upgrade.sh, with the feed on its own web server.
MSYS_NO_PATHCONV=1 docker run -d --rm --name "$name" --privileged --tmpfs /tmp \
	-v "$old:/pkg/old:ro" -v "$work:/work" \
	"openwrt/rootfs:x86_64-$version" \
	/sbin/init >/dev/null
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
	run 'logread 2>/dev/null | grep -q "init complete"' && break
	sleep 2
done
ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}/{{.IPPrefixLen}}{{end}}' "$name")
gateway=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}' "$name")
run '/etc/init.d/network stop; /etc/init.d/firewall stop; fw4 -q flush' >/dev/null 2>&1 || true
sleep 1
run "ip link set lo up; ip addr add 127.0.0.1/8 dev lo 2>/dev/null; ip link set eth0 up; ip addr flush dev eth0; ip addr add $ip dev eth0; ip route replace default via $gateway"
run 'mkdir -p /var/lock /www/feed; cp -r /work/feed/* /www/feed/; uhttpd -p 127.0.0.1:8000 -h /www'
sleep 1

# Changes one byte in the middle of a file on the web server, and keeps the original to put back.
flip() {
	run "f=$1; cp \$f /tmp/original; size=\$(wc -c < \$f); { head -c \$((size / 2)) /tmp/original; printf Z; tail -c +\$((size / 2 + 2)) /tmp/original; } > \$f; ! cmp -s \$f /tmp/original"
}
restore() { run "cp /tmp/original $1"; }

if [ "$kind" = opkg ]; then
	feed=/www/feed/opkg/amd64
	run 'echo "src/gz btproxy http://127.0.0.1:8000/feed/opkg/amd64" > /etc/opkg/customfeeds.conf'

	run 'opkg update >/dev/null 2>&1' || true
	if run 'opkg list | grep -q "^btproxy "'; then fail "listed the feed with no trusted key"; fi

	run 'cp /work/key.pub /etc/opkg/keys/$(usign -F -p /work/key.pub)'
	run 'opkg update >/dev/null 2>&1' || true
	run "opkg list | grep -q '^btproxy - $newversion-1'" || fail "the feed does not offer the new version with its key trusted"
	run "opkg list | grep -q '^luci-app-btproxy - $newversion-1'" || fail "the feed does not offer the LuCI page"

	run 'opkg install /pkg/old/btproxy-linux-amd64.ipk' >/dev/null 2>&1 || fail "installing the older package from a file"
	run 'opkg upgrade btproxy' >/dev/null 2>&1 || fail "upgrading from the feed"
	run "opkg list-installed | grep -q '^btproxy - $newversion-1'" || fail "the upgrade did not reach the new version"
	run 'opkg install --noaction luci-app-btproxy 2>&1 | grep -q "Installing luci-app-btproxy"' || fail "cannot install the LuCI page from the feed"

	run '/etc/init.d/btproxy stop; opkg remove btproxy' >/dev/null 2>&1
	flip "$feed/btproxy.ipk"
	if run 'opkg install btproxy' >/dev/null 2>&1; then fail "installed a tampered package"; fi
	if run 'opkg list-installed | grep -q "^btproxy "'; then fail "a tampered package ended up installed"; fi
	restore "$feed/btproxy.ipk"

	run "sed -i 's/^Description: .*/Description: tampered/' $feed/Packages; gzip -9nc $feed/Packages > $feed/Packages.gz"
	run 'opkg update >/dev/null 2>&1' || true
	if run 'opkg list | grep -q tampered'; then fail "believed a tampered index"; fi
else
	feed=/www/feed/apk/amd64
	apkversion=$(echo "$newversion" | tr - _)
	run 'echo "http://127.0.0.1:8000/feed/apk/amd64/packages.adb" >> /etc/apk/repositories.d/customfeeds.list'

	run 'apk update >/dev/null 2>&1' || true
	if run 'apk search -e btproxy 2>/dev/null | grep -q btproxy'; then fail "listed the feed with no trusted key"; fi

	run 'cp /work/key.pub /etc/apk/keys/btproxy.pem'
	run 'apk update >/dev/null 2>&1' || true
	run "apk search -e btproxy | grep -q '^btproxy-$apkversion-r1'" || fail "the feed does not offer the new version with its key trusted"
	run "apk search -e luci-app-btproxy | grep -q '^luci-app-btproxy-$apkversion-r1'" || fail "the feed does not offer the LuCI page"

	# A file install pins that file in /etc/apk/world, so moving onto the feed takes both of these commands.
	run 'apk add --allow-untrusted /pkg/old/btproxy-linux-amd64.apk' >/dev/null 2>&1 || fail "installing the older package from a file"
	run 'apk add btproxy && apk upgrade btproxy' >/dev/null 2>&1 || fail "moving onto the feed"
	run "apk list -I | grep -q '^btproxy-$apkversion-r1'" || fail "the upgrade did not reach the new version"
	run 'apk add --simulate luci-app-btproxy 2>&1 | grep -q "Installing luci-app-btproxy"' || fail "cannot install the LuCI page from the feed"

	run '/etc/init.d/btproxy stop; apk del btproxy' >/dev/null 2>&1
	package=$feed/btproxy-$apkversion-r1.apk
	flip "$package"
	if run 'apk add btproxy' >/dev/null 2>&1; then fail "installed a tampered package"; fi
	if run 'apk list -I | grep -q "^btproxy-"'; then fail "a tampered package ended up installed"; fi
	restore "$package"

	# A valid package, but not the one the index lists.
	run "cp $package /tmp/original; cp /pkg/old/btproxy-linux-amd64.apk $package"
	if run 'apk add btproxy' >/dev/null 2>&1; then fail "installed a package the index does not list"; fi
	restore "$package"

	flip "$feed/packages.adb"
	# The message depends on what the changed byte hit, such as "UNTRUSTED signature" or "ADB block error".
	said=$(run 'apk update 2>&1' || true)
	case $said in
	*"WARNING: updating"*127.0.0.1:8000*) ;;
	*) fail "believed a tampered index: $said" ;;
	esac
fi

echo "PASS: signed feed on OpenWrt $version ($kind)"
