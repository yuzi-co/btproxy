#!/bin/sh
# Installs, upgrades and removes the OpenWrt package inside a real OpenWrt root filesystem, under procd.
#
#   test/openwrt_package.sh <openwrt version> <dir with btproxy-linux-amd64.{ipk,apk} and luci-app-btproxy.{ipk,apk}> <dir with the older build>
#
# The radio is the fake backend, chosen through the settings file. Needs Docker and an x86_64 host.
set -eu

version=$1
. "$(dirname "$0")/openwrt_lib.sh"
new=$(abspath "$2")
old=$(abspath "$3")
name=btproxy-openwrt-test-$$
log_tail='logread | tail -20'
trap stop_router EXIT

boot_router -v "$new:/pkg/new:ro" -v "$old:/pkg/old:ro"
pkg_update >/dev/null
newpkg=/pkg/new/btproxy-linux-amd64.$ext
oldpkg=/pkg/old/btproxy-linux-amd64.$ext

# A package for another architecture says so, does not start, and can be replaced by the right one.
out=$(pkg_install "/pkg/new/btproxy-linux-mipsle.$ext" 2>&1) || true
echo "$out" | grep -q "does not run on this router" || fail "no explanation: $out"
if run '/etc/init.d/btproxy enabled'; then fail "wrong architecture enabled the service"; fi
if run 'pidof btproxy >/dev/null'; then fail "wrong architecture started the service"; fi
pkg_remove btproxy >/dev/null || fail "removing the wrong architecture"

# First install: the service is enabled and tries to start, without BlueZ, which is no longer a dependency.
run 'mkdir -p /etc && echo "{\"connections\": \"2\"}" > /etc/btproxy.json'
pkg_install "$oldpkg" >/dev/null || fail "install"
pkg_installed btproxy || fail "not installed"
run 'test -x /usr/bin/btproxy' || fail "no binary"
run 'test -f /etc/config/btproxy' || fail "no UCI config"
if run 'command -v bluetoothd >/dev/null'; then fail "bluez-daemon was pulled in"; fi
run '/etc/init.d/btproxy enabled' || fail "service not enabled"
sleep 3
run "test -f /lib/upgrade/keep.d/btproxy" || fail "no sysupgrade list"
# No adapter here: switch to the fake radio through UCI, which also shows that extra_args works.
fake='-backend fake -mdns=false -mac 02:00:00:00:00:01'
run "uci set btproxy.main.extra_args='$fake' && uci commit btproxy && /etc/init.d/btproxy restart"
sleep 3
run 'pidof btproxy >/dev/null' || fail "service not running"
run 'logread | grep -q "btproxy started"' || fail "no start in the log"
run 'logread | grep "btproxy started" | tail -1 | grep -q version=0.0.1' || fail "older build not running"

# UCI turns into flags, and a reload restarts the service with them.
run "uci set btproxy.main.name=uci-name && uci set btproxy.main.area=Attic && uci commit btproxy"
run '/etc/init.d/btproxy reload'
sleep 3
run 'cat /proc/$(pidof btproxy)/cmdline | tr "\0" " " | grep -q -- "-name uci-name -area Attic"' ||
	fail "UCI options did not reach the command line: $(run 'cat /proc/$(pidof btproxy)/cmdline | tr "\0" " "')"

# Empty options stay off the command line, so the settings page can manage those fields.
run 'cat /proc/$(pidof btproxy)/cmdline | tr "\0" " " | grep -q -- "-friendly-name"' &&
	fail "empty friendly name on the command line"
run 'cat /proc/$(pidof btproxy)/cmdline | tr "\0" " " | grep -q -- "-encryption-key-file"' &&
	fail "key flag without a key file"

run 'cat /proc/$(pidof btproxy)/cmdline | tr "\0" " " | grep -q -- "-bluetoothd-restart"' ||
	fail "no restart command by default"
run "uci set btproxy.main.bluetoothd_restart='' && uci commit btproxy && /etc/init.d/btproxy restart"
sleep 3
run 'cat /proc/$(pidof btproxy)/cmdline | tr "\0" " " | grep -q -- "-bluetoothd-restart"' &&
	fail "emptied restart command still passed"
run "uci set btproxy.main.bluetoothd_restart='/etc/init.d/bluetoothd restart' && uci commit btproxy"

# The backend option reaches the command line, and stays off it when empty.
run 'cat /proc/$(pidof btproxy)/cmdline | tr "\0" " " | grep -q -- "-backend kernel"' && fail "backend passed without being set"
run "uci set btproxy.main.backend=kernel && uci commit btproxy && /etc/init.d/btproxy restart"
sleep 3
# extra_args comes later and still picks the fake radio, so the proxy runs with no adapter here.
run 'cat /proc/$(pidof btproxy)/cmdline | tr "\0" " " | grep -q -- "-backend kernel"' ||
	fail "backend option not passed: $(run 'cat /proc/$(pidof btproxy)/cmdline | tr "\0" " "')"
run "uci -q delete btproxy.main.backend; uci commit btproxy"

# A key file turns on encryption.
run "(umask 077; echo $(head -c 32 /dev/urandom | base64) > /etc/btproxy.key)" # OpenWrt's busybox has no base64
run 'echo secret-settings-marker > /etc/btproxy.uipass'
run '/etc/init.d/btproxy restart'
sleep 3
run 'cat /proc/$(pidof btproxy)/cmdline | tr "\0" " " | grep -q -- "-encryption-key-file /etc/btproxy.key"' ||
	fail "key file not passed"
run 'logread | grep "btproxy started" | tail -1 | grep -q encryption=true' || fail "not encrypted with a key"

# Upgrade: settings stay, the new build runs, and a disabled service stays disabled.
key=$(run 'cat /etc/btproxy.key')
pkg_install "$newpkg" >/dev/null || fail "upgrade"
sleep 3
[ "$(run 'cat /etc/btproxy.key')" = "$key" ] || fail "upgrade changed the key"
run 'grep -q secret-settings-marker /etc/btproxy.uipass' || fail "upgrade changed the settings page password"
run 'grep -q connections /etc/btproxy.json' || fail "upgrade changed the settings file"
[ "$(run 'uci -q get btproxy.main.name')" = uci-name ] || fail "upgrade reset UCI"
run 'pidof btproxy >/dev/null' || fail "not running after the upgrade"
run 'logread | grep "btproxy started" | tail -1 | grep -qv version=0.0.1' || fail "older build still running"
run '/etc/init.d/btproxy enabled' || fail "upgrade disabled the service"

run '/etc/init.d/btproxy stop; /etc/init.d/btproxy disable'
pkg_install "$oldpkg" >/dev/null 2>&1 || true # downgrades are refused by some versions; only the next line matters
pkg_install "$newpkg" >/dev/null 2>&1 || true
sleep 2
run '/etc/init.d/btproxy enabled' && fail "reinstall enabled a disabled service"

# sysupgrade keeps what the package lists.
run 'sysupgrade -l 2>/dev/null | grep -q /etc/btproxy.key' || fail "sysupgrade drops the key"
run 'mkdir -p /etc/btproxy && echo {} > /etc/btproxy/bonds-test.json'
run 'sysupgrade -l 2>/dev/null | grep -q /etc/btproxy/bonds-test.json' || fail "sysupgrade drops the bonds"
run 'sysupgrade -l 2>/dev/null | grep -q /etc/config/btproxy' || fail "sysupgrade drops the UCI config"

# The LuCI page installs and removes on its own. Its access rules let it manage the key, but never read it.
pkg_install uhttpd "/pkg/new/luci-app-btproxy.$ext" >/dev/null || fail "LuCI page install"
run 'test -f /www/luci-static/resources/view/btproxy.js' || fail "no LuCI view"
run 'test -f /usr/share/luci/menu.d/luci-app-btproxy.json' || fail "no LuCI menu entry"
hash=$(run 'uhttpd -m acltest')
run "uci add rpcd login >/dev/null; uci set rpcd.@login[-1].username=acl; uci set rpcd.@login[-1].password='$hash'
	uci add_list rpcd.@login[-1].read=luci-app-btproxy; uci add_list rpcd.@login[-1].write=luci-app-btproxy
	uci commit rpcd; /etc/init.d/rpcd restart; sleep 1"
session=$(run "ubus call session login '{\"username\":\"acl\",\"password\":\"acltest\"}' | jsonfilter -e @.ubus_rpc_session")
[ -n "$session" ] || fail "no session"
access() { # scope, object, function, expected
	got=$(run "ubus call session access '{\"ubus_rpc_session\":\"$session\",\"scope\":\"$1\",\"object\":\"$2\",\"function\":\"$3\"}' | jsonfilter -e @.access")
	[ "$got" = "$4" ] || fail "access $1 $2 $3: $got, want $4"
}
access file /etc/btproxy.key read false
access ubus file read false
access file /etc/btproxy.key write true
access file /etc/btproxy.key list true
access file "/etc/init.d/btproxy restart" exec true
access file "/etc/init.d/btproxy enabled" exec true
access file "/sbin/logread -e btproxy" exec true
access file /bin/sh exec false
access file /etc/shadow read false
access uci btproxy write true
access uci network read false
pkg_remove luci-app-btproxy >/dev/null || fail "LuCI page removal"
if run 'test -e /www/luci-static/resources/view/btproxy.js'; then fail "LuCI view left behind"; fi
run 'test -x /usr/bin/btproxy' || fail "removing the LuCI page removed the proxy"

# Removal: the service stops and the binary goes.
run '/etc/init.d/btproxy enable; /etc/init.d/btproxy start'
sleep 2
pkg_remove btproxy >/dev/null || fail "remove"
sleep 2
if run 'pidof btproxy >/dev/null'; then fail "still running after removal"; fi
if run 'test -e /usr/bin/btproxy'; then fail "binary left behind"; fi
if run 'ls /etc/rc.d | grep -q btproxy'; then fail "start link left behind"; fi

echo "PASS: OpenWrt $version ($ext)"
