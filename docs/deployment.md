# Deployment

**A Dot rooted through EchoMuse arrives with the Alexa stack suppressed**, and
`deploy/restore-amazon.sh` undoes that.

`pm hide` and `pm disable` are independent, and EchoMuse applies both. A package
disabled *underneath* being hidden is not reported by `pm list packages -d` until
it is visible again, so the disabled set has to be read *after* unhiding: reading
it first found six packages and missed six more. The script verifies the result
rather than trusting `adb`, which exits 0 whatever happened remotely -- and
probes for root before it starts, because without it every read comes back empty
and empty is indistinguishable from "nothing left to fix".

`pm list packages -u` is "also uninstalled for this user", not "hidden", so the
difference from the plain listing is both sets at once, and it cannot say which
a survivor is. `dumpsys package` can: it reports `hidden=` and `installed=` per
user. Still `hidden=true` is a `pm unhide` that did not take, and fails the
restore. `installed=false` is somebody's own choice rather than EchoMuse's: it
is reported and not failed on, or a correct restore would end in RESTORE
INCOMPLETE and send them round again for nothing.

Counting the two sets instead of reading them was the first attempt, and it
fails a Dot that is already restored: everything left is then the owner's own,
so "none of them moved" and "there was nothing to move" have the same
arithmetic.

Every step takes its work from the device rather than from a plan made at the
start -- what to unhide, what to enable, what to verify. So the script resumes:
killed part way through, the next run reads the state it actually finds and does
what is left.

`com.amazon.device.software.ota` is left hidden on purpose. An OTA rewrites
`boot.img`, which removes Magisk and takes root and overdub with it.

`deploy/install.sh` pushes the binary to `/data/local/bin/` and the boot script
to Magisk's `service.d` (inside `magisk.img` on Magisk 17.3, hence the `/sbin`
path). That script is deliberately thin: Android has no user-level supervisor,
and `init` would need an `.rc` entry in a ramdisk this Magisk cannot patch, so
`service.d` is the hook and a shell loop is the restart. It supplies nothing:
the daemon carries its own audio and finds its own input node.

The daemon waits up to 60 seconds for its input node, because `service.d` runs
before the input drivers are certainly up. Without the wait a cold boot spends
its first restarts failing to open a node that is about to exist.

The loop counts its own restarts and truncates the log every twentieth, because
a daemon that exits immediately would otherwise append a failure every five
seconds for the rest of the boot. Counted rather than measured: this toolbox has
no `wc`.

`deploy/uninstall.sh` reverses that. The boot script goes first and alone,
because it is the only thing that starts the daemon at boot: a reboot part way
through then leaves a Dot with nothing running rather than a supervisor
respawning a half-deleted install.

**The binary goes before the kill, and that is not tidiness.** The supervisor is
a live shell loop holding its script as text, so deleting that file does not
reach it, and a kill on its own is answered five seconds later by a respawn. What
the loop can be told is whether the binary is still there, which is why it is
`while [ -x "$BIN" ]` and not `while true`. Removing a running executable is safe,
because the kernel keeps the inode until the last descriptor closes.

That leaves the loop itself unverified, and it cannot be checked the way the
daemon is: `ps` prints a shell's name as bare `sh` rather than the script it
runs, so no pattern over that listing finds it, and a `/proc/*/cmdline` scan
matches its own subshells. What the loop can be seen doing is recreating the
log, once per five-second cycle. So the log is removed, six seconds pass, and
the read-back separates a supervisor that outlived the uninstall from a file
that would not delete. Measured on a Dot whose running supervisor predated the
`-x` guard: the daemon was gone, every path was absent, the script reported
success, and the loop was still respawning.

Then SIGTERM, which reaches the handler that releases the grab. SIGKILL would do
as much, since the kernel drops the grab with the descriptor, so this is the
handler being used rather than needed. The state is read back afterwards for the
reason install.sh reads its own back: `adb shell` exits 0 whatever happened
remotely.

**`/data/local/bin` is chosen, not conventional.** No such directory exists on a
stock device, and every alternative is unavailable: Android has no `/usr`, `/` is
the boot ramdisk and is rebuilt from `boot.img` every boot, `/system` is read-only
and Magisk exists to leave it alone, and `/data/local/tmp` is `root:shell` scratch
and the one place an install can be wiped from under you. What is left is a
directory of our own under `/data`.
