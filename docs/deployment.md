# Deployment

## The name and the boot script

- The device name is an argument to `install.py`, not an edit to the tracked
  boot script. A name kept there is one `git checkout` from empty, and the next
  install pushes that over a working Dot. The running supervisor still holds the
  old arguments, so nothing fails until the next reboot.
- init runs the boot script, through `deploy/overdub.rc` in
  `/system/etc/init`. It is a `oneshot` service started on
  `sys.boot_completed`, so init never restarts it, and the script's own loop
  restarts the daemon.
- The loop runs in the foreground, so the service is the loop: `stop overdub`
  kills it, and `init.svc.overdub` reads `running` for as long as it runs. A
  loop backgrounded from a oneshot service would outlive the service, and init
  would report it stopped.
- The service runs `/system/bin/sh` with `seclabel u:r:adbd:s0`. SELinux
  enforces on Fire OS 6. init may not execute a file in `/data`
  (`shell_data_file`), and may not start a service in `su`. `adbd` and `su`
  are the only permissive domains, and init may start a service in `adbd`, so
  the shell starts there and runs the binary from `/data`. Each denial is
  logged as `permissive=1` and allowed.
- `/system` is read-only and verity is off: firebreak's Fire OS 6 unlock turns
  it off. `install.py` remounts `/` read-write to write the `.rc` file and
  remounts it read-only after. Fire OS 6 mounts system as root, so the remount
  is of `/`.
- init reads its `.rc` files only at boot. So the first install ends asking for
  a reboot. After that, `install.py` restarts the service: a SIGTERM to the
  daemon, so its handler withdraws the mDNS records and keeps the Sendspin
  delay, then `stop overdub`, a wait for `init.svc.overdub` to read
  `stopped`, and `start overdub`. The new boot script is read when the service
  starts, so a rename applies at once.
- The wait matters. `stop` returns before init reaps the loop, and a `start`
  that arrives first finds the service still running and does nothing; the
  reap then leaves it stopped.
- It restarts whenever the running daemon may not match what is installed: no
  applied stamp for it, a changed boot script, or a running `-name` other than
  the one pushed, read from `/proc/<pid>/cmdline`. So a rename run that died
  part way is finished by the next run.
- A changed `.rc` file is different: init keeps the definition it read at
  boot, so `install.py` prints `REBOOT REQUIRED` when it replaces one with
  other contents. Writing one where none was, after an uninstall, warns of
  nothing: init holds the definition the removed file gave it at boot, and
  nothing on the Dot records what that file said. If the reinstall changed
  `overdub.rc`, reboot.
- Every remount is read back from `/proc/mounts`, and a `/` left read-write is
  reported. A reboot mounts it read-only again.
- The boot script is thin on purpose. It supplies the name and the rollback;
  the daemon does everything else.
- It is replaced by rename, as the binary is. The service's shell reads it as it
  runs, so a copy over it would leave the shell at its old offset in the new
  text when the loop exits.
- The service started 12.7 seconds into a boot, about when
  `sys.boot_completed` came. The daemon waits up to 60 seconds for its input
  node.
- An update would replace `/system` and remove root and the `.rc` file.
  firebreak's Fire OS 6 unlock disables `otad` and points Amazon's update hosts
  at `127.0.0.1`, so a rooted Dot can go on Wi-Fi.
- The log is emptied each time the service starts: at boot, and at each
  restart `install.py` makes.
- The loop truncates the log every 20 restarts. Otherwise a daemon that exits at
  once -- no key, say -- appends a failure every 5 seconds for the whole boot.
  It counts restarts itself because this toolbox has no `wc`.
- Both scripts find the daemon by matching the last `ps` field against the
  binary's path. This works because this toolbox's `ps` prints `argv[0]` alone.
  A `ps` that printed the whole command line would match the name instead, and
  both scripts would report success having done nothing.

## Going back after an update

- An update leaves the binary it replaced at `bin/overdub.prev`, and
  `mapdump.jar.prev` beside the jar when it changed the jar. It also writes the
  trial marker, `bin/.overdub-trial`, holding the version on trial.
- While the marker exists, the loop counts each start. Before the 4th start it
  moves the previous binary and jar back, removes the marker and logs
  `the update to <version> exited 3 times`. So a new binary that exits at once
  is tried 3 times, about 15 seconds, and the Dot goes back to what worked.
- The count starts again whenever the marker's text changes. A binary that
  passed its trial and then installs the next update never leaves the loop an
  iteration without a marker, so a count kept across that would give the next
  update fewer tries. On the Dot, one such update was tried only twice.
- A binary passes its trial 60 seconds after the first client subscribes to
  its states, which is Home Assistant's last step in connecting. Then it
  removes the marker, both `.prev` files and the failure record. Time since
  start alone would pass a binary whose API is dead, or one that crashes only
  when Home Assistant connects and Home Assistant came back after 60 seconds.
  With Home Assistant away, the trial stays open: a further install waits, and
  any 3 exits roll back, however far apart. Home Assistant has just pressed
  **Install**, so it is away for the trial only when the new binary's API is
  broken, which is the case the trial is for, or when it went down at that
  moment.
- It removes them only when the marker still holds the text it found at start,
  so a marker for a newer update survives. A binary that hangs without exiting
  and keeps its API up is not caught: nothing else measures health.
- A binary that starts under a marker naming another version comes from an
  install that stopped between its renames: the jar is renamed before the
  binary. It puts back `mapdump.jar.prev`, if there is one, and removes the
  marker and `overdub.prev`. Passing that trial instead would keep the old
  binary with the new jar.
- When the loop goes back, it writes the failed version to
  `bin/.overdub-failed`. The restored binary then tells Home Assistant that
  the release it offers already failed once; without the record it would
  offer the same release as a clean update.
- With the marker present and no `overdub.prev`, the loop logs that, keeps the
  new binary and removes `mapdump.jar.prev`.
- `install.py` removes the marker, the failure record and both `.prev` files.
  Otherwise a binary it installed during a failing trial would be replaced by
  the loop's next rollback, after `install.py` had printed its md5 as verified.
- The loop sets `OVERDUB_ROLLBACK=1` for the daemon. The daemon reads it to know
  that the loop it runs under can go back. A boot script installed without a
  reboot does not give this: the running loop still holds the old script.
- The loop holds a counter, not a time, because a slow first start after a boot
  would look like a crash. A reboot during a trial starts the count again.
- Tested on the Dot: a binary that printed and exited 1 ran 3 times, and the
  4th start was the previous binary, by md5. A binary that ran removed the
  marker 60 seconds after a client subscribed.

## Updating over the API

- `internal/update` does what `install.py` does for the binary and the jar,
  from the Dot itself. docs/usage.md has what a user sees.
- It reads `releases/latest` from GitHub's API, then downloads `SHA256SUMS`,
  `SHA256SUMS.sig` and the tarball. Each read has a size limit, and the whole
  request has a 5-minute deadline. Go's TLS reads Fire OS's CA store, which
  trusts both roots GitHub uses (USERTrust ECC for the API, ISRG Root X1 for
  the downloads): a 3.2 MB release tarball downloaded on the Dot.
- The signature must check, the tarball's sha256 must match the line that
  names `overdub-<tag>.tar.gz`, and the tarball must hold the binary, the jar
  and the boot script under `overdub-<tag>/`.
- The release's boot script, with this Dot's name put in as `install.py` does
  it, must equal the installed one byte for byte. An update does not replace
  the boot script. A broken one would stop the daemon at the next boot, where
  no rollback reaches.
- The daemon refuses an install unless the loop set `OVERDUB_ROLLBACK=1`, and
  while the trial marker exists. A binary still on trial would otherwise
  remove the new marker and `.prev` when its own trial passed, leaving a
  failing update with nothing to go back to.
- A power cut between the jar's rename and the binary's leaves the old binary
  under the new version's marker, with the new jar. It finds the marker names
  another version and goes back to its own jar, as "Going back after an
  update" says.
- Each new file is written beside its target, synced, read back and renamed
  into place. The old binary and jar are hard-linked to their `.prev` names
  first. The jar is replaced only when it changed, and is owned by 32051.
- After the rename the daemon shuts down as it does on SIGTERM, and the loop
  starts the new binary 5 seconds later.
- `install.py`'s applied stamp then names a pid and a binary md5 that do not
  match, so the next `install.py` restarts the daemon once.
- `releasesURL` is a variable so that a test build can point it somewhere
  else: `OVERDUB_VERSION='v0.2.9 -X main.releasesURL=http://127.0.0.1:8099/latest'`
  stamps both, because `build.sh` passes the variable inside `-ldflags`.
- Tested on the Dot that way, with a release served from its loopback and
  signed with the release key, and driven by aioesphomeapi: `v0.2.9` updated to
  `v0.3.0` in 7 seconds, with a changed jar owned by 32051, and the trial passed.
  A signed `v0.4.0` whose binary exited at once ran 3 times, and the loop went
  back to `v0.3.0` by md5.

## What is pushed, and how it is verified

| file | on the Dot | owner, mode | staged in | checked by |
|---|---|---|---|---|
| `overdub` | `bin/overdub` | root, `0755` | `tmp` | md5, `-x` |
| boot script | `bin/overdub.sh` | root, `0755` | `tmp` | md5, `-x` |
| init service | `/system/etc/init/overdub.rc` | root, `0644` | `tmp` | md5 |
| API key | `bin/.overdub-noise-key` | root, `0600` | `0700` dir | text, mode |
| adb public key | `bin/adb_keys` | root, `0644` | `0700` dir | text |

`bin` is `/data/local/bin`, `0700`. `tmp` is `/data/local/tmp`. Every
command runs through root adb: firebreak's Fire OS 6 unlock leaves an adbd
that runs as root, and no `su`.

- `/data/local/bin` exists on no stock device. Only the `.rc` file goes in
  `/system`: an update rewrites `/system`, and the binary is updated from the
  Dot itself, so everything else lives in `/data`.
- Fire OS 6 has `md5sum` and no `md5`, and no busybox; `od` and `ls` are
  toybox's.
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
  Alexa for about 5 seconds. A changed boot script restarts the init service
  rather than the daemon, because the loop holds the arguments it started
  with: killing the daemon alone would respawn the old name.
- A repair counts as a change, so that run does not end `Already installed`:
  a mode on `/data/local/bin`, the API key or the boot script.
- A run that changed nothing but warned, such as a daemon not running or a
  rename awaiting a reboot, ends `Nothing changed; see the warning above.`
  instead, so the last line does not read as success.
- An install cannot revoke. adbd authenticates against
  `/data/misc/adb/adb_keys`, which the daemon writes only on Secure, and
  `ro.adb.secure` is not persistent. An install with no key stops Secure being
  offered, and a Dot already in Secure keeps the old key until it reboots.

## Uninstall

`uninstall.py` works in this order:

1. Delete the `.rc` file, alone, remounting `/` to do it. It is the only thing
   that starts the daemon at boot, so a reboot part way through leaves nothing
   running.
2. Delete the boot script, the binary, the previous binary, the trial marker,
   the failure record, the keys, the applied stamp, the staging directory and
   `/data/local/map`.
   The binary goes before the kill because the supervisor is a live shell
   loop: deleting the boot script does not stop it, and it respawns a killed
   daemon 5 seconds later. The loop runs `while [ -x "$BIN" ]`, so removing
   the binary ends it. The kernel keeps a running executable's inode until the
   last descriptor closes.
3. Kill the daemon with SIGTERM, which reaches the handler that releases the
   grab, withdraws the mDNS records and keeps the Sendspin delay. With the
   binary gone the loop then ends, and `stop overdub` after the wait is only a
   safeguard: `stop` kills with SIGKILL, so the handler would not run.
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
  stderr into stdout and a linker warning would read as a leftover.
- The Sendspin paths are Go constants too. `main_test.go` checks that
  `install.py` names `sendspinKeyPath`, and that `uninstall.py` names it,
  `sendspinPairsPath` and `sendspinLastPath`.
- The ports are Go constants (`apiPort`, `sendspin.Port`), which Python cannot
  read, so the script assigns its own constants. `main_test.go` compares them
  against both constants. The read-back alone would show a moved port only as a
  rule that would not delete.

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
- The release also publishes `SHA256SUMS.sig`, an ed25519 signature over
  `SHA256SUMS`. The binary embeds the public key, `deploy/release-signing.pub`,
  and installs an update only when the signature checks. The attestation would
  prove the same, but checking it needs sigstore, a dependency far larger than
  the 64 bytes this costs.
- The private key is `RELEASE_SIGNING_KEY`, in PEM, a secret of the `release`
  environment. The environment admits only `v*` tags. A repository secret
  would reach every workflow run on a branch of this repository, and any
  collaborator can push one whose `ci.yml` prints it.
- The job signs with `openssl pkeyutl -rawin`, then verifies against the
  committed public key, so a missing or wrong secret fails the release before
  anything is published.
- A new key pair means a new binary. Dots running the old one refuse every
  release signed by the new key, so they need one `install.py`.
- `SHA256SUMS` names the tarball, and the tarball's name carries the tag. So
  the signature binds the hash to a version. A signed older release cannot
  pass for a newer one only while the daemon checks both halves: that the name
  it finds is the tag it fetched, and that the tag is newer than its own.
- `THIRD-PARTY.txt` is in the tarball because the licences compiled into the
  binary ask for it there. Source distribution does not need it.
- The tarball carries no rooting script: rooting is firebreak's. Nothing
  before the release runs the tarball's copies, so CI lists the tarball and
  checks that each of its 3 scripts is there and executable.
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
