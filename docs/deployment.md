# Deployment

## The name and the boot script

- The device name is an argument to `install.py`, not an edit to the tracked
  boot script. A name kept there is one `git checkout` from empty, and the next
  install pushes that over a working Dot. The running supervisor still holds the
  old arguments, so nothing fails until the next reboot.
- A rename takes effect only after a reboot. The boot script runs once, at boot,
  and the shell loop that respawns the daemon holds its arguments. After a kill,
  the loop restarts the daemon under the old name.
- `install.py` reads the running daemon's `/proc/<pid>/cmdline` and prints
  `REBOOT REQUIRED` when the name does not match what it pushed.
- The boot script is thin on purpose. Android has no user-level supervisor, and
  `init` would need an `.rc` entry in a ramdisk this Magisk cannot patch, so
  `service.d` is the hook and a shell loop is the restart. It supplies the name;
  the daemon does everything else.
- The daemon waits up to 60 seconds for its input node, because `service.d` can
  run before the input drivers are up.
- The loop truncates the log every 20 restarts. Otherwise a daemon that exits at
  once -- no key, say -- appends a failure every 5 seconds for the whole boot.
  It counts restarts itself because this toolbox has no `wc`.
- Both scripts find the daemon by matching the last `ps` field against the
  binary's path. This works because this toolbox's `ps` prints `argv[0]` alone.
  A `ps` that printed the whole command line would match the name instead, and
  both scripts would report success having done nothing.

## What is pushed, and how it is verified

| file | on the Dot | owner, mode | staged in | checked by |
|---|---|---|---|---|
| `overdub` | `bin/overdub` | root, `0755` | `tmp` | md5, `-x` |
| boot script | `service.d/overdub.sh` | root, `0755` | `tmp` | md5, `-x` |
| API key | `bin/.overdub-noise-key` | root, `0600` | `0700` dir | text, mode |
| adb public key | `bin/adb_keys` | root, `0644` | `0700` dir | text |
| `mapdump.jar` | `/data/local/map/` | 32051, `0644` | `tmp` | md5, 32051 `-r` |

`bin` is `/data/local/bin`, `0700`. `service.d` is
`/sbin/.core/img/.core/service.d`. `tmp` is `/data/local/tmp`.
`/data/local/map` is `0755`, owned by 32051.

- `/data/local/bin` exists on no stock device, and every alternative is
  unusable: there is no `/usr`, `/` is the boot ramdisk and is rebuilt every
  boot, `/system` is read-only and Magisk leaves it alone, and
  `/data/local/tmp` is `root:shell` scratch.
- The jar is not in `bin`: MAP's uid loads it and cannot traverse `0700`.
  `/data/local` is already `o+x`. Ownership matters rather than the path:
  dalvik may write beside the jar.
- A jar that uid 32051 cannot reach fails much later, with a message about a
  class. So the check runs `[ -r ]` as that uid rather than parsing `ls`.
  Toolbox `ls -ln` prints no link count, busybox's does, and Magisk puts its
  busybox first on `PATH`. `[ -r ]` covers the file mode and the directory
  traversal together.
- The API pre-shared key is generated on the installing machine and printed
  once. An install that finds a key keeps it, so a reinstall does not lock Home
  Assistant out. The check "is there a key already" is the only one whose wrong
  answer destroys something, and it fails closed both ways. The key step runs
  before the binary is pushed, so a Dot whose key cannot be settled keeps the
  binary it had.
- `adb push` does not carry the local mode, and `/data/local/tmp` is
  world-traversable, so both keys go through a `0700` directory. The API key's
  is read back to confirm it is gone, and `install.py` removes it on any exit
  it can catch: an error, Ctrl-C, SIGTERM or SIGHUP, and Ctrl-Break on Windows.
- The first of those signals turns all of them off before cleanup starts. A
  closing terminal can send SIGHUP twice, and the second would otherwise kill
  cleanup, or the `adb` removing the key. Windows delivers Ctrl-C to every
  process on the console, so there cleanup also turns Ctrl-C off with
  `SetConsoleCtrlHandler`, which the `adb` it starts inherits.
- Each `adb` call in cleanup gives up after 30 seconds and prints the command
  to finish by hand. With interrupts off, a Dot that stopped answering would
  otherwise hold cleanup with no way out.
- Closing the console window on Windows ends the process without cleanup:
  Python gets no signal it can act on. A key left that way is never printed,
  and the next install keeps it. Delete it and install again.
- `uninstall.py` removes the staging directory too, because a copy left there
  is the live key.
- The adb public key is not a secret. Pushed straight to `tmp` it lands `0666`
  under an `o+x` directory, so another uid could swap in its own key between
  the push and the copy. The read-back would catch that only after the
  stranger's key was in place.
- An empty or malformed adb key is refused before anything touches the Dot,
  so a refusal cannot follow a freshly printed API key. An empty key would pass
  every read-back, because every one of no lines is on the device. Installed, it would set `ro.adb.secure` against a key that
  authenticates nobody, which applies to USB too and locks the operator out.
- Each file is compared with the Dot's copy before it is pushed: by md5, and
  the adb key by its text. A match skips the push, not the read-back after it.
- The daemon restarts only when the running one may not match what is
  installed. After each verified restart, `install.py` writes
  `bin/.overdub-applied`: the new pid and the md5s of the binary, the API key
  and the adb key. A later run skips the restart only when the running pid and
  all three md5s match it.
- A stamp is written only after a restart, so a run stopped before its restart
  leaves an old one, and the next run restarts. A reboot changes the pid, so
  the first install after one restarts once.
- File times cannot decide this. The mtime of `/proc/<pid>` is set when the
  kernel creates its inode, which can happen again under memory pressure, and
  the Dot's clock is wrong until network time.
- The daemon reads the API key once, at start, and Home Assistant learns the
  Network ADB options only when it connects, so a change to either key needs a
  restart to reach it.
- A restart drops Home Assistant and Music Assistant and hands the button to
  Alexa for about 5 seconds, and a new boot script alone gains nothing from
  one: the supervisor holds the arguments it started with. So a rename is
  reported as REBOOT REQUIRED without a kill.
- A repair counts as a change, so that run does not end `Already installed`:
  a mode on `/data/local/bin`, the API key or the boot script, or an owner or
  mode under `/data/local/map`. Those are read from `ls -ln`, which prints a
  link count under busybox and none under toolbox, so both shapes are parsed.
- A run that changed nothing but warned, such as a daemon not running or a
  rename awaiting a reboot, ends `Nothing changed; see the warning above.`
  instead, so the last line does not read as success.
- An install cannot revoke. adbd authenticates against
  `/data/misc/adb/adb_keys`, which the daemon writes only on Secure, and
  `ro.adb.secure` is not persistent. An install with no key stops Secure being
  offered, and a Dot already in Secure keeps the old key until it reboots.

## Uninstall

`uninstall.py` works in this order:

1. Delete the boot script, alone. It is the only thing that starts the daemon
   at boot, so a reboot part way through leaves nothing running.
2. Delete the binary, the keys, the applied stamp, the staging directory and
   `/data/local/map`.
   The binary goes before the kill because the supervisor is a live shell
   loop: deleting the boot script does not stop it, and it respawns a killed
   daemon 5 seconds later. The loop runs `while [ -x "$BIN" ]`, so removing
   the binary ends it. The kernel keeps a running executable's inode until the
   last descriptor closes.
3. Kill the daemon with SIGTERM, which reaches the handler that releases the
   grab. SIGKILL would also work, since the kernel drops the grab with the
   descriptor.
4. Delete the log, wait 6 seconds, and sweep for leftovers. `ps` cannot find
   the loop: it shows as bare `sh`, and a `/proc/*/cmdline` scan matches its
   own subshells. The loop recreates the log every 5-second cycle, so a log
   that came back means the supervisor survived.
5. Delete the firewall rules, once the daemon is confirmed dead. The daemon
   re-asserts `tcp/6053` every 30 seconds and `tcp/8928` while Sendspin is on,
   so an earlier delete would be undone. Each rule is deleted in a loop,
   because the chain is not ours alone, then read back. A rule left behind is
   reported, not failed on: nothing listens behind it, and a reboot clears it.
6. Clear every `persist.overdub.*` property and remove its file, then read
   back the switch flag. Each property changes what the next install does: a
   Dot switched off through Home Assistant would come back off, and a tuned
   output delay would come back. The script enumerates
   `/data/property/persist.overdub.*`, so a new property needs no change here.

- The echoed leftovers are filtered in two passes. Fixed paths match exactly.
  The Sendspin key's temporary siblings match by prefix, because a run killed
  between the write and the link leaves one under a random suffix.
- A path swept but not filtered is dropped silently and reported as removed. So
  every path in the sweep must also be in the filter. The two staging files
  are not swept, and are removed without being verified.
- The same sweep runs first. With nothing swept present and no daemon, there
  is no supervisor to wait out, so the 6-second wait is skipped. A firewall
  rule deleted or a property cleared still counts as a removal, so a Dot
  left with only those ends `Overdub uninstalled.` with its notes.
  Otherwise the run ends `Already uninstalled; nothing to remove.`, or
  `Nothing removed; see the warning above.` when it warned.
- A run that stops at STILL SUPERVISED exits before the notes. The reboot
  that ends the loop leaves nothing, so the next run ends `Already
  uninstalled` and the notes are never printed. docs/usage.md's Uninstalling
  section says the same.
- The filter is an allowlist, not "anything echoed", because `adb` merges
  stderr into stdout and a linker warning from `su` would read as a leftover.
- The ports are Go constants (`apiPort`, `sendspin.Port`), which Python cannot
  read, so the script assigns its own constants. `main_test.go` compares them
  against both constants. The read-back alone would show a moved port only as a
  rule that would not delete.

## What Alexa's session leaves on the device

- Routing Alexa's session through the daemon writes outside this daemon's own
  namespace: two properties, `persist.amazon.scl.host` and
  `persist.amazon.scl.port`, and one file under `/system`, which needs `/system`
  remounted.
- So uninstall has three steps the rest of the daemon does not need. Each is read
  back, and a failure warns rather than passing quietly:
  1. Clear both properties, and delete their files under `/data/property`. A
     property left set with nothing behind it is a Dot that cannot answer.
  2. Remove the file the daemon added under `/system`. It always lands under the
     same name, so the path is named directly rather than kept in a file.
  3. Delete `/data/local/bin/.overdub-avs-identity`, which is what the daemon
     made for itself on first run.
- The identity is deleted with the binary, in the first sweep.

## Releases

- A release is a tarball. Building needs an NDK, a JDK and an Android SDK, so it
  carries the built binary and jar beside the scripts.
- `install.py` builds when `build.sh` is beside it, and installs `build/overdub`
  as it stands when it is not. The test is whether the file exists, not
  whether it can run; docs/pitfalls.md says why.
- The tarball's binary cannot be rebuilt from what is beside it and compared.
  So the release publishes `SHA256SUMS` and a provenance attestation naming the
  workflow run and commit, and `-version` names the tag. The source path stays
  the default, because none of that equals building it.
- `THIRD-PARTY.txt` is in the tarball because the licences compiled into the
  binary ask for it there. Source distribution does not need it.
- The tarball also carries `dot_firmware.py`. Nothing before the release
  runs the tarball's copies, so CI lists the tarball and checks that each of
  its 4 scripts is there and executable.
- The two triggers do not overlap. A push to a fork raises no event here, so
  `pull_request` tests a contributor's work. `pull_request` never fires for a
  tag, so `push` (main and `v*` tags) cuts a release and tests the merge onto
  main. Unfiltered, every branch with an open PR ran the suite twice.
- The release is a job in `ci.yml`, not a workflow of its own, so it can say
  `needs: [build, dot-scripts, pre-commit]`. `needs:` does not reach across
  workflows. A tag on a red commit produces nothing.
- The attestation names `.github/workflows/ci.yml` as the signer, which is what
  `--signer-workflow` wants. Moving the job would change what old and new
  releases attest to.
- A tag with a hyphen is published as a prerelease. Semver marks a prerelease
  with a hyphen, and GitHub infers nothing from the tag, so an rc would
  otherwise take the Latest banner.
- A release that fails part way needs a hand. `gh release create` is not
  idempotent, so a re-run stops at "a release with the same tag name already
  exists". Delete the draft, then re-run the job from its run page, because
  pushing the existing tag again raises no event. Uploading over what exists
  would let a re-run replace the assets of a published release. The release is
  created as a draft and published only once both assets are up.
- A second workflow on `workflow_run` runs with the default branch as its ref,
  so `$GITHUB_REF_NAME` stops naming the tag and the attestation records
  `refs/heads/main`. It also fires for pull requests from forks, and the release
  job holds `contents: write` and `attestations: write`.
