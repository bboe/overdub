package device

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var fixtureToken = "Atnr|" + strings.Repeat("0123456789abcdef", 21) + "0123456789ab"

func useMapDB(t *testing.T, path string) {
	t.Helper()
	wasPath, wasGap := mapDBPath, mapReadGap
	mapDBPath, mapReadGap = path, time.Millisecond
	t.Cleanup(func() { mapDBPath, mapReadGap = wasPath, wasGap })
}

func TestTheTokenIsReadOutOfTheMapStore(t *testing.T) {
	useMapDB(t, "testdata/registered.db")
	token, err := AlexaToken()
	if err != nil {
		t.Fatalf("AlexaToken: %v", err)
	}
	if token != fixtureToken {
		t.Errorf("the token read is %d bytes starting %.10q, want the %d-byte fixture token",
			len(token), token, len(fixtureToken))
	}
	if registered, ok := AlexaRegistered(); !registered || !ok {
		t.Errorf("AlexaRegistered() = %v, %v; want true, true: the credential is the registration",
			registered, ok)
	}
}

func TestAValueSplitOverPagesComesBackWhole(t *testing.T) {
	useMapDB(t, "testdata/registered.db")
	key, _, err := mapValue("device_private_key")
	if err != nil {
		t.Fatal(err)
	}
	want := "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("MIIEpAIBAAKCAQEA", 160) +
		"\n-----END RSA PRIVATE KEY-----\n"
	if string(key) != want {
		t.Errorf("a %d-byte value in a 1 KB-page store read back as %d bytes; the overflow"+
			" pages were not followed to the end", len(want), len(key))
	}
	adp, _, err := mapValue("adp_token")
	if err != nil {
		t.Fatal(err)
	}
	if want := "{enc:" + strings.Repeat("A", 2600) + "}"; string(adp) != want {
		t.Errorf("a %d-byte value read back as %d bytes", len(want), len(adp))
	}
	cert, _, err := mapValue("device_cert")
	if err != nil {
		t.Fatal(err)
	}
	want = "-----BEGIN CERTIFICATE-----\n" + strings.Repeat("Q", 990) + "\n-----END CERTIFICATE-----\n"
	if string(cert) != want {
		t.Errorf("a %d-byte value, just over what a page holds, read back as %d bytes: SQLite"+
			" keeps only the minimum share on the page for that one", len(want), len(cert))
	}
}

func TestTheStoreIsReadWhateverItsPageSize(t *testing.T) {
	useMapDB(t, "testdata/default-pages.db")
	if token, err := AlexaToken(); err != nil || token != fixtureToken {
		t.Errorf("a 4 KB-page store gave %.10q, %v", token, err)
	}
}

func TestAStoreWithoutTheTokenIsAnUnregisteredDot(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
	}{
		{"a store with no refresh_token", "testdata/unregistered.db"},
		{"an empty store", "testdata/empty.db"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useMapDB(t, tt.path)
			if registered, ok := AlexaRegistered(); registered || !ok {
				t.Errorf("AlexaRegistered() = %v, %v; want false, true", registered, ok)
			}
			if _, err := AlexaToken(); err == nil {
				t.Error("AlexaToken gave a token where there is none")
			}
		})
	}
}

func TestAStoreThatCannotBeReadIsNoReading(t *testing.T) {
	db, err := os.ReadFile("testdata/registered.db")
	if err != nil {
		t.Fatal(err)
	}
	wal := append([]byte(nil), db...)
	wal[18], wal[19] = 2, 2
	utf16 := append([]byte(nil), db...)
	utf16[59] = 2
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"a file that is not sqlite", []byte(strings.Repeat("not sqlite ", 200))},
		{"a store in WAL mode, whose rows may sit in the log", wal},
		{"a UTF-16 store, whose keys would never match", utf16},
		{"no store at all", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "map.db")
			if tt.data != nil {
				if err := os.WriteFile(path, tt.data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			useMapDB(t, path)
			if registered, ok := AlexaRegistered(); ok || registered {
				t.Errorf("AlexaRegistered() = %v, %v; want a reading nobody took: it says"+
					" nothing about the account, and \"not registered\" would hide the box",
					registered, ok)
			}
			if _, err := AlexaToken(); err == nil {
				t.Error("AlexaToken gave a token from a store it should not trust")
			}
		})
	}
}

func TestAStoreMidWriteIsNotRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "map.db")
	db, err := os.ReadFile("testdata/registered.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, db, 0o600); err != nil {
		t.Fatal(err)
	}
	hot := append(append([]byte(nil), journalMagic...), make([]byte, 500)...)
	if err := os.WriteFile(path+"-journal", hot, 0o600); err != nil {
		t.Fatal(err)
	}
	useMapDB(t, path)
	if _, err := AlexaToken(); err == nil {
		t.Error("a store with a hot journal was read; half a transaction can hold half a token")
	}
	if registered, ok := AlexaRegistered(); ok || registered {
		t.Errorf("AlexaRegistered() = %v, %v during a write; want no reading", registered, ok)
	}
	spent := make([]byte, len(hot))
	if err := os.WriteFile(path+"-journal", spent, 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := AlexaToken(); err != nil || token != fixtureToken {
		t.Errorf("with a zeroed journal beside it the read gave %.10q, %v: PERSIST mode leaves"+
			" the file, and only its header says whether a transaction is open", token, err)
	}
	os.Remove(path + "-journal")
	if token, err := AlexaToken(); err != nil || token != fixtureToken {
		t.Errorf("after the journal went the read gave %.10q, %v", token, err)
	}
}

func TestADamagedStoreIsAnErrorAndNeverAPanic(t *testing.T) {
	db, err := os.ReadFile("testdata/registered.db")
	if err != nil {
		t.Fatal(err)
	}
	try := func(label string, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s: the reader panicked: %v", label, r)
			}
		}()
		sqliteLookup(data, mapTable, mapTokenKey)
	}
	for cut := 0; cut < len(db); cut++ {
		try("truncated at "+strconv.Itoa(cut), db[:cut])
	}
	flipped := make([]byte, len(db))
	for i := 0; i < len(db); i++ {
		copy(flipped, db)
		flipped[i] ^= 0xff
		try("byte "+strconv.Itoa(i)+" flipped", flipped)
		copy(flipped, db)
		flipped[i] = 0xff
		try("byte "+strconv.Itoa(i)+" set", flipped)
	}
	if _, found, err := sqliteLookup(db, "nothing", mapTokenKey); err != nil || found {
		t.Errorf("a table the store lacks gave found=%v, %v; a store with no table holds no"+
			" row, as an empty store does", found, err)
	}
	if _, found, err := sqliteLookup(db, mapTable, "no_such_key"); err != nil || found {
		t.Errorf("a key the store lacks gave found=%v, %v", found, err)
	}
}

func TestEveryStepOfTheVarintIsRead(t *testing.T) {
	for _, tt := range []struct {
		in   []byte
		want uint64
		n    int
	}{
		{[]byte{0x00}, 0, 1},
		{[]byte{0x7f}, 127, 1},
		{[]byte{0x81, 0x00}, 128, 2},
		{[]byte{0x82, 0x61}, 353, 2},
		{[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, ^uint64(0), 9},
		{[]byte{0x80}, 0, 0},
		{nil, 0, 0},
	} {
		if v, n := varint(tt.in); v != tt.want || n != tt.n {
			t.Errorf("varint(%x) = %d, %d; want %d, %d", tt.in, v, n, tt.want, tt.n)
		}
	}
}
