# Things that fail silently

**A file mode is invisible to everything else here.** gofmt, vet, the tests and
shellcheck all read the shell scripts rather than execute them, and the build
runs `./build.sh` and never `install.sh`. So a script that lost its executable
bit passes the whole suite and fails for the first person to run the command
README.md gives them.

CI asserts that every tracked `.sh` is `100755`, as a rule rather than a list,
so a script added later is covered without being remembered. Measured the hard
way: `install.sh` was once committed `100644`, and the whole suite passed.

**`install.sh` can lie, so it verifies itself.** `cp` onto the running binary
fails with ETXTBSY, silently: toolbox `cp` prints "Text file busy" and exits 0,
and `adb shell` exits 0 whatever happened remotely, so `set -e` catches nothing.
An install can report success, change nothing, and take a reboot to notice. The
binary is replaced by rename and its md5 checked against the build.

**By hash rather than size, because size does not separate two builds.** A
binary replaced during an install here measured byte-for-byte the same size as
the one replacing it, so a silently failed `cp` would have passed a size check.
`build.sh` pins `-buildvcs=false` to make the comparison mean something: Go
otherwise stamps the commit, its timestamp and a dirty flag into the binary, all
three fixed-length, so builds of different source differ in content while
matching exactly in size.

Without the stamp the same tree always gives the same bytes -- measured across
repeat builds, a different directory, a checkout with no `.git`, and a dirty
tree.

That last one is the one to say out loud, so `install.sh` does. Reproducibility
here means the binary cannot tell you what it was built from, and `git checkout`
carries modified files across a branch change: the checkout reads as a revert,
the build is not one, and every check downstream then passes on the wrong
binary, twice in a row, printing "binary verified" each time. Measured the hard
way.

Every step is read back, because not one of them reports its own failure: the
binary is hashed, and the boot script compared against what was pushed. The boot
script matters most -- it is the only reason anything runs at all, and the path
it goes to is the one this device's Magisk 17.3 uses. A Magisk that keeps
`service.d` somewhere else has no such directory, so the `cp` fails, the `rm`
beside it runs regardless, and the Dot does not come back from its next reboot
with no log to read, because that script is what creates it. Which versions
share the 17.3 layout is not something measured here.

The restart check goes through a function for the same class of reason: a
pipeline reports only its last command, so with `adb` inline a pulled cable read
as an empty pid, which is what this script says when nothing was supervising the
daemon at all.
