package sendspin

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flynn/noise"
)

func TestLoadOrCreateKeysCreatesThenReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".overdub-sendspin-key")
	made, created, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKeys: %v", err)
	}
	if !created {
		t.Error("first call did not report that it created the keys")
	}
	again, created, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKeys reload: %v", err)
	}
	if created {
		t.Error("second call reported that it created the keys")
	}
	if !bytes.Equal(made.Identity.Private, again.Identity.Private) {
		t.Error("reload produced a different private key")
	}
	if !bytes.Equal(made.Identity.Public, again.Identity.Public) {
		t.Error("reload produced a different public key")
	}
	if !bytes.Equal(made.PairingPSK, again.PairingPSK) {
		t.Error("reload produced a different pairing psk")
	}
}

func TestCreatedKeyFileIsUnreadableByAnyoneElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".overdub-sendspin-key")
	if _, _, err := LoadOrCreateKeys(path); err != nil {
		t.Fatalf("LoadOrCreateKeys: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != keyFileMode {
		t.Errorf("key file is %#o, want %#o", perm, keyFileMode)
	}
}

func TestLoadOrCreateKeysRefusesAReadableKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".overdub-sendspin-key")
	if _, _, err := LoadOrCreateKeys(path); err != nil {
		t.Fatalf("LoadOrCreateKeys: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	_, _, err := LoadOrCreateKeys(path)
	if err == nil {
		t.Fatal("loaded a key file every uid on the device can read")
	}
	if !strings.Contains(err.Error(), "pairing psk") {
		t.Errorf("error does not say what is at stake: %v", err)
	}
}

func TestLoadOrCreateKeysRefusesAShortFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".overdub-sendspin-key")
	if err := os.WriteFile(path, make([]byte, keyFileLen-1), keyFileMode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := LoadOrCreateKeys(path); err == nil {
		t.Fatal("accepted a short key file")
	}
}

func TestTwoDevicesGetDifferentKeys(t *testing.T) {
	a, _, err := LoadOrCreateKeys(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatalf("LoadOrCreateKeys: %v", err)
	}
	b, _, err := LoadOrCreateKeys(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatalf("LoadOrCreateKeys: %v", err)
	}
	if bytes.Equal(a.PairingPSK, b.PairingPSK) {
		t.Error("two installs share a pairing psk")
	}
	if bytes.Equal(a.Identity.Private, b.Identity.Private) {
		t.Error("two installs share a static private key")
	}
}

func TestStoredPrivateKeyAgreesWithTheNoiseLibrary(t *testing.T) {
	pair, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	id, err := identityFromPrivate(pair.Private)
	if err != nil {
		t.Fatalf("identityFromPrivate: %v", err)
	}
	if !bytes.Equal(id.Public, pair.Public) {
		t.Fatalf("public key derived as %x, noise says %x", id.Public, pair.Public)
	}
}

func TestKeyFileHoldsThePrivateKeyThenThePSK(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".overdub-sendspin-key")
	keys, _, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKeys: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(raw[:keyLen], keys.Identity.Private) {
		t.Error("the first 32 bytes are not the static private key")
	}
	if !bytes.Equal(raw[keyLen:], keys.PairingPSK) {
		t.Error("the last 32 bytes are not the pairing psk")
	}
}

func TestLoadOrCreateKeysRefusesAnAllZeroFile(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  []byte
	}{
		{"both halves", make([]byte, keyFileLen)},
		{"the psk alone", append(bytes.Repeat([]byte{7}, keyLen), make([]byte, pskLen)...)},
		{"the private key alone", append(make([]byte, keyLen), bytes.Repeat([]byte{7}, pskLen)...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".overdub-sendspin-key")
			if err := os.WriteFile(path, c.raw, keyFileMode); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if _, _, err := LoadOrCreateKeys(path); !errors.Is(err, errZeroKey) {
				t.Errorf("err = %v, want %v; a zero psk is one every operator can guess",
					err, errZeroKey)
			}
		})
	}
}

func TestLoadOrCreateKeysRefusesEveryBitPastTheOwner(t *testing.T) {
	// One mode per bit, because a check written against any single one of them looks
	// right: 0604 is world-readable and 0640 is group-readable, and the pairing token
	// is the whole of what either buys.
	for _, mode := range []fs.FileMode{0o604, 0o640, 0o660, 0o606, 0o644, 0o610, 0o601} {
		t.Run(fmt.Sprintf("%#o", mode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".overdub-sendspin-key")
			if _, _, err := LoadOrCreateKeys(path); err != nil {
				t.Fatalf("LoadOrCreateKeys: %v", err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("Chmod: %v", err)
			}
			if _, _, err := LoadOrCreateKeys(path); err == nil {
				t.Errorf("loaded a key file at %#o", mode)
			}
		})
	}
}

func TestLoadOrCreateKeysRefusesALongFile(t *testing.T) {
	// Only the short case was covered, so a length check written as "at least" would
	// have read a key out of the first 64 bytes of anything.
	path := filepath.Join(t.TempDir(), ".overdub-sendspin-key")
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, keyFileLen+1), keyFileMode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := LoadOrCreateKeys(path); err == nil {
		t.Fatal("loaded a key file longer than the key")
	}
}

func TestALostCreateLoadsTheKeyTheWinnerWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".overdub-sendspin-key")
	const racers = 16
	type result struct {
		keys Keys
		err  error
	}
	out := make(chan result, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			keys, _, err := LoadOrCreateKeys(path)
			out <- result{keys, err}
		}()
	}
	close(start)

	var first []byte
	for i := 0; i < racers; i++ {
		r := <-out
		if r.err != nil {
			t.Fatalf("a caller that lost the create did not fall back to the load: %v", r.err)
		}
		if first == nil {
			first = r.keys.PairingPSK
			continue
		}
		if !bytes.Equal(r.keys.PairingPSK, first) {
			t.Fatal("two callers ended up with different identities for one key file")
		}
	}
}

func TestTheClientIDIsThePublicKeyAndNotThePrivateOne(t *testing.T) {
	// Both halves are 32 bytes and both encode to 43 characters, so returning the
	// wrong one is invisible to any test that checks a length or compares the value
	// against itself. client_id goes out in cleartext in every client/init.
	path := filepath.Join(t.TempDir(), ".overdub-sendspin-key")
	keys, _, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKeys: %v", err)
	}
	id := keys.Identity.ClientID()
	if id == EncodeID(keys.Identity.Private) {
		t.Fatal("client_id is the static private key: the daemon broadcasts its own" +
			" identity to anything that opens a socket")
	}
	if id != EncodeID(keys.Identity.Public) {
		t.Errorf("client_id = %q, want the public key %q", id, EncodeID(keys.Identity.Public))
	}
}

func TestLoadOrCreateKeysRefusesSomethingThatIsNotAFile(t *testing.T) {
	// A fifo at the key path used to hang the daemon before the check was reached,
	// because the read came first. Only root can plant one in a 0700 directory, so
	// what this really buys is an error instead of a wedged boot.
	dir := t.TempDir()
	path := filepath.Join(dir, ".overdub-sendspin-key")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := LoadOrCreateKeys(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a fifo was accepted as a key file")
		}
		if !strings.Contains(err.Error(), "regular file") {
			t.Errorf("err = %v, want it to say what the file is not", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadOrCreateKeys blocked on a fifo: the read runs before the check")
	}

	sub := filepath.Join(dir, "adirectory")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, _, err := LoadOrCreateKeys(sub); err == nil {
		t.Error("a directory was accepted as a key file")
	}
}

func TestTheTwoHalvesOfTheKeyFileDoNotShareOneArray(t *testing.T) {
	raw := make([]byte, keyFileLen)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	keys, err := decodeKeys(raw)
	if err != nil {
		t.Fatalf("decodeKeys: %v", err)
	}
	psk := append([]byte(nil), keys.PairingPSK...)

	// An append to the private key must not reach the psk. Both halves came out of
	// one read, so a slice of it has room to spare and writes into its neighbour.
	_ = append(keys.Identity.Private, 0xff)
	if !bytes.Equal(keys.PairingPSK, psk) {
		t.Errorf("appending to the private key rewrote the psk: %x -> %x",
			psk[:4], keys.PairingPSK[:4])
	}

	// Nor may either half alias the caller's buffer, which a caller may reuse or
	// wipe once the keys are decoded.
	for i := range raw {
		raw[i] = 0
	}
	if allZero(keys.Identity.Private) {
		t.Error("the private key aliases the buffer it was read from")
	}
	if allZero(keys.PairingPSK) {
		t.Error("the pairing psk aliases the buffer it was read from")
	}
}
