# Hard constraints

**`GOARCH=arm` is not optional.** FireOS 5 on biscuit runs a 32-bit ARM
userspace even though the kernel is arm64. `struct input_event` is 16 bytes there
and 24 in a 64-bit userspace, so a wrong-arch build would desynchronise every
evdev read rather than fail. `internal/evdev` asserts the 32-bit `syscall.Timeval`
at compile time, so a 64-bit build does not compile at all. The guard is over
the word size rather than the architecture, so `GOARCH=386` compiles and passes
and nothing here would run on it. `build.sh` is the convenience, not the guard:
anything that compiles the tree, `go vet` included, needs the target set.

**A flag has to earn its place.** The binary takes no arguments at all: every
fact about biscuit is a `const` in `main.go`. A flag whose only correct value is
already known is configuration that can be got wrong for no gain.
