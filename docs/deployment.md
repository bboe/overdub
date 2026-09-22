# Deployment

## The name and the boot script

- The device name is an argument to `install.sh`, not an edit to the tracked
  boot script. A name kept there is one `git checkout` from empty, and the next
  install pushes that over a working Dot. The running supervisor still holds the
  old arguments, so nothing fails until the next reboot.
- A rename takes effect only after a reboot. The boot script runs once, at boot,
  and the shell loop that respawns the daemon holds its arguments. After a kill,
  the loop restarts the daemon under the old name.
- `install.sh` reads the running daemon's `/proc/<pid>/cmdline` and prints
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

## Coming from EchoMuse

README.md has the steps. This is why `deploy/restore-amazon.sh` works as it
does.

- `pm hide` and `pm disable` are independent, and EchoMuse applies both.
- `pm list packages -d` does not report a package that is disabled while hidden.
  So the script reads the disabled set after it unhides.
- `pm list packages -u` means "also uninstalled for this user", not "hidden".
  Its difference from the plain listing mixes both sets. `dumpsys package`
  reports `hidden=` and `installed=` per user:
  - `hidden=true` means `pm unhide` did not take, and fails the restore.
  - `installed=false` is the owner's own choice. It is reported and does not
    fail the restore.
- The script reads each survivor rather than counting. On a Dot already
  restored, everything left is the owner's, and a count cannot tell "none
  moved" from "nothing to move".
- Every step reads its work from the device, not from a plan made at the start.
  A run killed part way through resumes on the next run.

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
  is read back to confirm it is gone, and the trap removes it on any exit.
  `uninstall.sh` removes it too, because a copy left there is the live key.
- The adb public key is not a secret. Pushed straight to `tmp` it lands `0666`
  under an `o+x` directory, so another uid could swap in its own key between
  the push and the copy. The read-back would catch that only after the
  stranger's key was in place.
- An empty or malformed adb key is refused before it is pushed. An empty key
  passes every read-back, because an empty pattern matches the blank line the
  device echoes. Installed, it would set `ro.adb.secure` against a key that
  authenticates nobody, which applies to USB too and locks the operator out.
- An install cannot revoke. adbd authenticates against
  `/data/misc/adb/adb_keys`, which the daemon writes only on Secure, and
  `ro.adb.secure` is not persistent. An install with no key stops Secure being
  offered, and a Dot already in Secure keeps the old key until it reboots.

## Uninstall

`uninstall.sh` works in this order:

1. Delete the boot script, alone. It is the only thing that starts the daemon
   at boot, so a reboot part way through leaves nothing running.
2. Delete the binary, the keys, the staging directory and `/data/local/map`.
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
  every path in the sweep must also be in the filter. The adb key and the two
  staging files are not swept, and are removed without being verified.
- The filter is an allowlist, not "anything echoed", because `adb` merges
  stderr into stdout and a linker warning from `su` would read as a leftover.
- The ports are Go constants (`apiPort`, `sendspin.Port`), which shell cannot
  read, so the script assigns its own variables. `main_test.go` compares them
  against both constants. The read-back alone would show a moved port only as a
  rule that would not delete.

## Releases

- A release is a tarball. Building needs an NDK, a JDK and an Android SDK, so it
  carries the built binary and jar beside the scripts.
- `install.sh` builds when `build.sh` is beside it, and installs `build/overdub`
  as it stands when it is not. The test is whether the file exists, not
  whether it can run; docs/pitfalls.md says why.
- The tarball's binary cannot be rebuilt from what is beside it and compared.
  So the release publishes `SHA256SUMS` and a provenance attestation naming the
  workflow run and commit, and `-version` names the tag. The source path stays
  the default, because none of that equals building it.
- `THIRD-PARTY.txt` is in the tarball because the licences compiled into the
  binary ask for it there. Source distribution does not need it.
- The two triggers do not overlap. A push to a fork raises no event here, so
  `pull_request` tests a contributor's work. `pull_request` never fires for a
  tag, so `push` (main and `v*` tags) cuts a release and tests the merge onto
  main. Unfiltered, every branch with an open PR ran the suite twice.
- The release is a job in `ci.yml`, not a workflow of its own, so it can say
  `needs: build`. `needs:` does not reach across workflows. A tag on a red
  commit produces nothing.
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
