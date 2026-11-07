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
. "$(dirname "$0")/openwrt_lib.sh"
old=$(abspath "$2")
new=$(abspath "$3")
name=
log_tail='logread | grep -a btproxy | tail -8'
trap stop_router EXIT
newpkg=/pkg/new/btproxy-linux-amd64.$ext

# Starts a router with the older package, BlueZ brought in as its dependency, and bluetoothd running.
start_router() {
	stop_router
	name=btproxy-upgrade-test-$$-$1
	boot_router -v "$new:/pkg/new:ro" -v "$old:/pkg/old:ro"
	pkg_update >/dev/null
	pkg_install "/pkg/old/btproxy-linux-amd64.$ext" >/dev/null 2>&1 || fail "installing the older package"
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
pkg_install "$newpkg" >/dev/null 2>&1 || fail "upgrade"
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
if [ "$pkg" = apk ]; then
	pkg_add bluez-daemon >/dev/null 2>&1 || fail "asking for BlueZ by name"
	pkg_install "$newpkg" >/dev/null 2>&1 || fail "upgrade with BlueZ kept"
	run 'test -x /etc/init.d/bluetoothd' || fail "BlueZ asked for by name was removed"
	run 'pidof bluetoothd >/dev/null' || fail "the upgrade stopped a bluetoothd that stays"
	choice=$(first_choice)
	case $choice in
	*"bluetoothd is running"*) ;;
	*) fail "did not stay on BlueZ: $choice" ;;
	esac
else
	pkg_install "$newpkg" >/dev/null 2>&1 || fail "upgrade"
	pkg_add bluez-daemon >/dev/null 2>&1 || fail "installing BlueZ again"
	run 'test -x /etc/init.d/bluetoothd' || fail "BlueZ did not come back"
fi

echo "PASS: upgrade on OpenWrt $version ($ext)"
