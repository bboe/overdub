# Deployment

**The device name is an argument, not an edit.** A name kept in the tracked boot
script is one `git checkout` away from being empty again, and the install that
follows pushes that over a working Dot. Nothing complains at the time, because
the supervisor still holds the old arguments; the device simply does not come
back from its next reboot. The reboot check compares what was pushed rather than
what is tracked, for the same reason.

**A Dot rooted through EchoMuse arrives with the Alexa stack suppressed**, and
`deploy/restore-amazon.sh` undoes that.

`pm hide` and `pm disable` are independent, and EchoMuse applies both. A package
disabled *underneath* being hidden is not reported by `pm list packages -d` until
it is visible again, so the disabled set has to be read *after* unhiding: reading
it first found six packages and missed six more. The script verifies the result
rather than trusting `adb`, which exits 0 whatever happened remotely, and
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
start: what to unhide, what to enable, what to verify. So the script resumes:
killed part way through, the next run reads the state it actually finds and does
what is left.

`com.amazon.device.software.ota` is left hidden on purpose. An OTA rewrites
`boot.img`, which removes Magisk and takes root and overdub with it.

**A release is a tarball, and `install.sh` reads its own surroundings to know
it.** Building this needs an NDK, a JDK and an Android SDK between them, for a
device whose whole audience is people who have already rooted one, so the
release carries the built binary and the built jar beside the scripts that
install them. There is no flag for that. `install.sh` builds when `build.sh` is
next to it and installs `build/overdub` as it stands when it is not, which is a
fact about the directory rather than an assertion anybody can get wrong: a
source tree always has both, and the tarball deliberately has neither the
toolchain nor the script that would drive one. The test is whether that file
exists rather than whether it can be run, and docs/pitfalls.md says what the
other reading installs. Everything downstream is unchanged, hash check included
-- what it compares against is simply a file that was built elsewhere.

The workflow's two triggers cover different things and are filtered so they do
not overlap. A push to a fork raises no event in this repository, so
`pull_request` is what tests a contributor's work; `pull_request` never fires
for a tag, so `push` is what cuts a release, and what tests the merge onto
main. Unfiltered, every branch with a pull request open ran the whole suite
twice, which is also why the first draft of the gate had to reason about two
`build` results for one commit.

The release is a job in the CI workflow rather than a workflow of its own, and
that is what lets it be gated. `needs:` does not reach across workflows, so two
files would leave the release racing the tests it is meant to wait for, with
nothing but a poll of the checks API to make it wait. In one file it says
`needs: build` and a tag on a red commit produces nothing.

The attestation names the workflow that produced the file, so every release is
signed by `.github/workflows/ci.yml`. That path is what `--signer-workflow`
wants from anyone hardening the check beyond what README.md shows, and it is a
promise to keep: moving the release job to a file of its own again would change
what old and new releases attest to, and split a check that names one path.

A tag carrying a hyphen is published as a prerelease, because semver says a
hyphen is what marks one and GitHub infers nothing from the tag: `v0.1.0-rc1`
and `v0.1.0-rc2` were both published as full releases, so both took the Latest
release banner and the download link that follows it. What decides is the shape
of the tag rather than a flag remembered at the time.

A release that fails part way through needs a hand, deliberately. `gh release
create` is not idempotent, so re-running the job for a tag stops at "a release
with the same tag name already exists": delete the draft it left, then re-run
the job from its own run page, because the tag already exists and pushing it
again raises no event to trigger anything. The alternative is for the step to
upload over whatever it finds, which would let a re-run silently replace the
assets of a release that was already published, and a release whose bytes can
change quietly is worth less than one that needs a deliberate deletion. Nothing
is public in the meantime, because the draft is only published once both assets
are up.

The alternative, a second workflow on `workflow_run`, costs more than it looks.
Such a job runs with the default branch as its ref, so the tag survives only as
event data: `$GITHUB_REF_NAME` stops naming it, and the provenance attestation
would record `refs/heads/main` rather than the tag, which is the one field a
consumer checks. It also fires for pull requests from forks, and this job holds
`contents: write` and `attestations: write`.

What the tarball loses is the one thing a source install gets for free: the
binary cannot be rebuilt from what is beside it and compared. So the release
publishes `SHA256SUMS` and a provenance attestation, which say which workflow
run and which commit produced the file, and `-version` says which tag the
running daemon came from. None of that is as good as having built it, which is
why the source path stays the documented default and the tarball says so.

`THIRD-PARTY.txt` is in the tarball because the licences compiled into the
binary ask for it there. Source distribution never needed it; this is the first
build published as a binary, and it is the case those clauses are about.

`deploy/install.sh` pushes the binary to `/data/local/bin/` and the boot script
to Magisk's `service.d` (inside `magisk.img` on Magisk 17.3, hence the `/sbin`
path). That script is deliberately thin: Android has no user-level supervisor,
and `init` would need an `.rc` entry in a ramdisk this Magisk cannot patch, so
`service.d` is the hook and a shell loop is the restart. It supplies the name;
everything else the daemon does for itself.

The third is the API's pre-shared key, and it is the only pushed thing that is a
secret. It is generated on the installing machine rather than the Dot, printed
once, and never printed again: an install that finds one keeps it, so
reinstalling does not lock Home Assistant out of a Dot it was talking to. Which
means the branch that decides "is there one already" is the only check in either
script whose wrong answer destroys something, and it is written to fail closed in
both directions rather than one. The whole key step runs before anything is
pushed, so a device whose key cannot be settled keeps the binary it had, and the
message saying nothing changed is true.

Staging is where the secret is exposed rather than where it lands. `adb push`
does not carry the local mode and `/data/local/tmp` is world-traversable, so the
key goes through a `0700` directory of the installer's own making, is read back
to confirm that directory is gone, and is reaped by the trap on any exit that
happens in between. `uninstall.sh` removes that directory too, because a copy
left there is the live key and nothing else would ever look for it.

The fourth pushed thing is the operator's adb public key, which is what
`Secure` authenticates against. It is not a secret, and it goes through a `0700`
directory of ours anyway: `adb push` lands a file `0666` and `/data/local/tmp`
is `o+x`, so any uid could substitute a key of its own between the push and the
copy, and the read-back would catch that only after a stranger's key was already
where the daemon looks. An empty file is the one shape that would pass every
check, because the read-back greps for the file's own content and an empty
pattern matches the blank line the device echoes; installed, it would set
`ro.adb.secure` against a key that authenticates nobody, which reaches USB too
and locks the operator out of a Dot with no screen to say so. So an empty key is
refused before anything is pushed.

What this cannot do is revoke. adbd authenticates against
`/data/misc/adb/adb_keys`, which only the daemon writes and only on `Secure`,
and `ro.adb.secure` is not persistent -- so an install with no key stops
`Secure` being offered from here on, and a Dot already in `Secure` keeps
honouring the key it was given until it reboots.

The daemon waits up to 60 seconds for its input node, because `service.d` runs
before the input drivers are certainly up. Without the wait a cold boot spends
its first restarts failing to open a node that is about to exist.

Both scripts find the daemon by matching the last field of `ps` against the
binary's path, which keeps working now the boot script passes `-name` because
this toolbox's `ps` prints `argv[0]` alone. A `ps` that printed the whole
command line would match the name instead, and both scripts would then take
their "no daemon" branch and report success having done nothing.

The loop counts its own restarts and truncates the log every twentieth, because
a daemon that exits immediately, if it had no key for example, would otherwise
append a failure every five seconds for the rest of the boot. Counted rather
than measured: this toolbox has no `wc`.

`deploy/uninstall.sh` reverses that, key included. The boot script goes first and
alone,
because it is the only thing that starts the daemon at boot: a reboot part way
through then leaves a Dot with nothing running rather than a supervisor
respawning a half-deleted install.

**A rename does not take until the Dot reboots.** The boot script is read once,
at boot; what respawns the daemon afterwards is a shell loop already holding the
arguments it was started with. So `install.sh <new name>` writes the new script,
kills the daemon, and the loop brings it back under the old name. Measured: two
Dots reinstalled under new names went on reporting the old ones until they were
rebooted.

`install.sh` already reads the running daemon's own `/proc/<pid>/cmdline` back
and prints `REBOOT REQUIRED` when the name it finds is not the one it was given,
which is the only part of an install that a reboot is needed to finish. What is
worth saying is what happened anyway: the line was printed and went unread,
because the output was being filtered for other words. A check that reports
correctly is only half of one.

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

The tcp/6053 rule goes last, and after the daemon is confirmed dead rather than
before: the daemon re-asserts that rule every thirty seconds, so a deletion
taken earlier would be undone before the next line of the script ran. It is
deleted in a loop, because the chain is not ours alone and one pass proves
nothing, and then read back. A rule left behind is reported rather than failed
on: nothing listens behind it once the daemon is gone, and it does not survive a
reboot in any case.

The port is a `const` in `serve.go` and a literal in the script, because shell
cannot read a Go constant. A test compares the two: left to the read-back alone,
a port that moved would surface as a rule that would not delete, which says
nothing about why.

**`/data/local/bin` is chosen, not conventional.** No such directory exists on a
stock device, and every alternative is unavailable: Android has no `/usr`, `/` is
the boot ramdisk and is rebuilt from `boot.img` every boot, `/system` is read-only
and Magisk exists to leave it alone, and `/data/local/tmp` is `root:shell` scratch
and the one place an install can be wiped from under you. What is left is a
directory of our own under `/data`.

**`mapdump.jar` cannot live there, and gets a directory of its own.** The jar is
loaded by MAP's uid rather than by the daemon, and `/data/local/bin` is root's
alone -- `0700`, asserted by the install rather than inherited, after three Dots
were measured carrying two different modes -- so the jar goes to
`/data/local/map`, owned by 32051 and `0755`.
`/data/local` is already `o+x` on this build, so that uid can reach it by name.
docs/pitfalls.md has what it looks like when this is got wrong, which is not a
permission error, and why the varying mode is the thing not to build on.

Ownership is the part that matters rather than the path: dalvik may want to
write beside the jar, and on this build it does not -- the directory holds the
jar and nothing else -- but the jar has to be readable by the uid that runs it.

`install.sh` reads the jar back like everything else it pushes: by hash, and
then by asking uid 32051 whether it can read it, because a jar that landed
correctly somewhere that uid cannot reach fails at the far end of a command with
a message about a class.

The question is asked rather than inferred from `ls`. The uid is not a column
anybody can count on -- Android's toolbox `ls -ln` prints no link count and
busybox's does, and Magisk puts its own busybox first on `PATH` -- and the uid
was only ever a proxy for the thing that matters. `[ -r ]` as that uid covers the
file's mode and the directory's traversal at once. Measured on a Dot: with the
directory root-owned and `0700`, which is the arrangement that broke, the answer
is `no`; owned by 32051 it is `yes`, and `0700` owned by 32051 is also `yes`,
because owner permissions are what that uid gets.
