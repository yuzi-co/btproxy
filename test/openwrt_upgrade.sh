#!/bin/sh
# Upgrades from a release whose package depends on BlueZ, with bluetoothd running, inside a real OpenWrt root
# filesystem. The new proxy must not choose BlueZ when the package manager is about to remove it.
#
#   test/openwrt_upgrade.sh <openwrt version> <dir with the older btproxy-linux-amd64 package> <dir with the new one>
#
# A container has no Bluetooth in the kernel, so a script stands in for bluetoothd. The kernel names a script's
# process after the script, which is what the proxy looks for. Needs Docker and an x86_64 host.
set -eu

version=$1
abspath() { (cd "$1" && { pwd -W 2>/dev/null || pwd; }); } # pwd -W: the path Docker needs under Git Bash
old=$(abspath "$2")
new=$(abspath "$3")

case $version in
24.*|23.*) ext=ipk ;;
*) ext=apk ;;
esac

name=
fail() {
	echo "FAIL ($version): $*" >&2
	docker exec "$name" sh -c 'logread | grep -a btproxy | tail -8' >&2 || true
	exit 1
}
# Each command gets 3 minutes, so a hang fails with the command that hung instead of blocking the job. The message
# goes to the script's own stderr, kept as fd 3, since callers often send a command's output to /dev/null.
exec 3>&2
run() {
	run_rc=0
	MSYS_NO_PATHCONV=1 timeout -k 10 180 docker exec "$name" sh -c "$1" || run_rc=$?
	[ $run_rc -ne 124 ] || echo "FAIL ($version): timed out after 180 s: $1" >&3
	return $run_rc
}
stop_container() { [ -z "$name" ] || docker stop -t 1 "$name" >/dev/null 2>&1 || true; }
trap stop_container EXIT

# Starts a router with the older package, BlueZ brought in as its dependency, and bluetoothd running.
start_router() {
	stop_container
	name=btproxy-upgrade-test-$$-$1
	MSYS_NO_PATHCONV=1 docker run -d --rm --name "$name" --privileged --tmpfs /tmp \
		-v "$new:/pkg/new:ro" -v "$old:/pkg/old:ro" \
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
	run "ip link set eth0 up; ip addr flush dev eth0; ip addr add $ip dev eth0; ip route replace default via $gateway"
	run 'mkdir -p /var/lock'
	if [ "$ext" = ipk ]; then
		run 'opkg update >/dev/null'
		add='opkg install'
	else
		run 'apk update >/dev/null'
		add='apk add --allow-untrusted'
	fi
	run "$add /pkg/old/btproxy-linux-amd64.$ext" >/dev/null 2>&1 || fail "installing the older package"
	run '/etc/init.d/btproxy stop'
	prog=$(run 'command -v bluetoothd')
	run "printf '#!/bin/sh\nwhile :; do sleep 60; done\n' > $prog; chmod 755 $prog; /etc/init.d/bluetoothd start; sleep 2"
	run 'pidof bluetoothd >/dev/null' || fail "the stand-in bluetoothd is not running"
	run 'mkdir -p /var/lib/bluetooth/test; echo bond > /var/lib/bluetooth/test/info'
	run '/etc/init.d/btproxy start'
}

# The older release logs no choice of backend, so the first such line is the new build's first start. Later ones say
# nothing here: without D-Bus the bluez backend exits and procd starts it again, which hides a wrong first choice.
first_choice() {
	sleep 3
	run 'logread | grep -a "backend chosen" | head -1'
}

# BlueZ installed only as a dependency goes away with the upgrade, so the proxy must not choose it.
start_router dependency
run "$add /pkg/new/btproxy-linux-amd64.$ext" >/dev/null 2>&1 || fail "upgrade"
if run 'test -e /etc/init.d/bluetoothd'; then fail "BlueZ stayed, which this test does not expect"; fi
choice=$(first_choice)
case $choice in
*"bluetoothd is running"*) fail "chose BlueZ just before it was removed: $choice" ;;
"") fail "the new build never chose a backend" ;;
esac
run 'test -f /var/lib/bluetooth/test/info' || fail "the upgrade lost BlueZ's bonds, which the kernel backend copies"
run 'test -x /usr/bin/btproxy' || fail "no binary after the upgrade"

# Asked for by name, BlueZ stays and the proxy keeps using it. opkg has no way to ask before the upgrade, so there it
# is installed again afterwards.
start_router kept
if [ "$ext" = apk ]; then
	run 'apk add bluez-daemon' >/dev/null 2>&1 || fail "asking for BlueZ by name"
	run "$add /pkg/new/btproxy-linux-amd64.apk" >/dev/null 2>&1 || fail "upgrade with BlueZ kept"
	run 'test -x /etc/init.d/bluetoothd' || fail "BlueZ asked for by name was removed"
	run 'pidof bluetoothd >/dev/null' || fail "the upgrade stopped a bluetoothd that stays"
	choice=$(first_choice)
	case $choice in
	*"bluetoothd is running"*) ;;
	*) fail "did not stay on BlueZ: $choice" ;;
	esac
else
	run "$add /pkg/new/btproxy-linux-amd64.ipk" >/dev/null 2>&1 || fail "upgrade"
	run 'opkg install bluez-daemon' >/dev/null 2>&1 || fail "installing BlueZ again"
	run 'test -x /etc/init.d/bluetoothd' || fail "BlueZ did not come back"
fi

echo "PASS: upgrade on OpenWrt $version ($ext)"
