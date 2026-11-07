# Shared by the OpenWrt container tests, which source it once $version is set. $name must hold the router container
# before run() is called. A script that wants the router's log on a failure sets log_tail to the command that prints
# it, after sourcing.

# Each command gets this long, so a hang fails with the command that hung instead of blocking the job.
limit=180

# GNU timeout, which macOS has only as gtimeout from Homebrew's coreutils. Without it the commands run untimed, and a
# hang blocks until the CI job's own limit.
if command -v timeout >/dev/null 2>&1; then
	timeout_cmd=timeout
elif command -v gtimeout >/dev/null 2>&1; then
	timeout_cmd=gtimeout
else
	timeout_cmd=
	echo "notice: no timeout command, so a hung command blocks until the job's own limit" >&2
fi

# Runs a Docker command, killed after the number of seconds given first when the host has a timeout command.
timed() {
	if [ -n "$timeout_cmd" ]; then
		MSYS_NO_PATHCONV=1 "$timeout_cmd" -k 10 "$@"
	else
		shift
		MSYS_NO_PATHCONV=1 "$@"
	fi
}

# Messages go to the script's own stderr, kept as fd 3, since callers often send a command's output to /dev/null or
# capture it together with stderr.
exec 3>&2
fail() {
	echo "FAIL ($version): $*" >&3
	[ -z "${log_tail:-}" ] || timed 30 docker exec "$name" sh -c "$log_tail" >&3 2>&3 || true
	exit 1
}

# Runs a command in the router. A timeout ends the script rather than counting as a failure the caller may tolerate:
# docker exec does not pass the signal on, so the hung command keeps running in the container, and nothing after it
# can be trusted.
run() {
	run_rc=0
	timed "$limit" docker exec "$name" sh -c "$1" || run_rc=$?
	if [ $run_rc -eq 124 ]; then
		diagnose
		fail "timed out after $limit s: $1"
	fi
	return $run_rc
}

# Says, after a timeout, whether the container still answers at all and what runs in it: the upgrade test has hung
# in logread after the package upgrade on CI, with nothing to tell a stuck docker exec from a stuck logd.
diagnose() {
	timed 15 docker inspect -f 'diag: container {{.State.Status}}, pid {{.State.Pid}}' "$name" >&3 2>&3 || true
	if timed 15 docker exec "$name" true >/dev/null 2>&1; then
		echo "diag: docker exec answers; processes (pid comm state ppid):" >&3
		timed 15 docker exec "$name" sh -c 'for s in /proc/[0-9]*/stat; do cut -d" " -f1-4 "$s"; done' >&3 2>&3 || true
		echo "diag: ubus log object:" >&3
		timed 15 docker exec "$name" ubus call log read '{"lines":3}' >&3 2>&3 || echo "diag: ubus log read failed or hung" >&3
		# Where procd and logd are blocked, and what they hold open: logd stopped answering ubus in the hangs seen
		# so far, with a btproxy zombie that procd had not reaped.
		timed 15 docker exec "$name" sh -c 'for p in 1 $(pidof logd) $(pidof ubusd); do
			echo "diag: pid $p $(cat /proc/$p/comm) wchan $(cat /proc/$p/wchan) stack:"; cat /proc/$p/stack
			ls -l /proc/$p/fd | sed "s/^.* \([0-9]* ->\)/  \1/"; done
			echo "diag: unix sockets:"; cat /proc/net/unix' >&3 2>&3 || true
	else
		echo "diag: docker exec itself does not answer" >&3
	fi
}
