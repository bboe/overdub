package device

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const a2dpDump = `Profile: HeadsetService
  mCurrentDevice: null
  mTargetDevice: null
Profile: A2dpService
  mCurrentDevice: F8:5C:7D:39:15:31
  mTargetDevice: null
  mPlayingA2dpDevice: F8:5C:7D:39:15:31
  StateMachine: A2dpStateMachine:
curState=Connected

Profile: A2dpSinkService
  mCurrentDevice: null
`

func TestParseA2DPAddress(t *testing.T) {
	for _, tt := range []struct {
		name    string
		dump    string
		address string
		ok      bool
	}{
		{"a speaker is connected", a2dpDump, "F8:5C:7D:39:15:31", true},
		{"nothing is connected",
			"Profile: A2dpService\n  mCurrentDevice: null\n", "", true},
		{"the sink profile does not answer for the source",
			"Profile: A2dpSinkService\n  mCurrentDevice: F8:5C:7D:39:15:31\n", "", false},
		{"a headset does not answer for it either",
			"Profile: HeadsetService\n  mCurrentDevice: F8:5C:7D:39:15:31\n", "", false},
		{"no a2dp profile in the dump", "enabled: true\nstate: 12\n", "", false},
		{"a device that is not an address",
			"Profile: A2dpService\n  mCurrentDevice: nonsense\n", "", false},
		{"an address with more after it",
			"Profile: A2dpService\n  mCurrentDevice: F8:5C:7D:39:15:31:00\n", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			address, ok := parseA2DPAddress(tt.dump)
			if address != tt.address || ok != tt.ok {
				t.Errorf("address = %q, %v; want %q, %v", address, ok, tt.address, tt.ok)
			}
		})
	}
}

const btConfig = `<Bluedroid>
    <N1 Tag="Local">
        <N1 Tag="Adapter">
            <N2 Tag="Address" Type="string">88:71:e5:b6:44:09</N2>
            <N9 Tag="Name" Type="string">Echo Dot-U8T</N9>
        </N1>
    </N1>
    <N2 Tag="Remote">
        <N1 Tag="46:06:27:c5:bd:45">
            <N1 Tag="DevType" Type="int">2</N1>
        </N1>
        <N505 Tag="f8:5c:7d:39:15:31">
            <N1 Tag="Timestamp" Type="int">1789975764</N1>
            <N2 Tag="Name" Type="string">JBL Go 3</N2>
            <N3 Tag="DevClass" Type="int">2360340</N3>
        </N505>
        <N506 Tag="00:02:5b:00:15:10">
            <N2 Tag="Name" Type="string">Office Shield</N2>
        </N506>
    </N2>
</Bluedroid>
`

func TestBluetoothName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bt_config.xml")
	if err := os.WriteFile(path, []byte(btConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name    string
		address string
		want    string
		ok      bool
	}{
		{"the paired speaker", "F8:5C:7D:39:15:31", "JBL Go 3", true},
		{"an address the config does not carry", "00:00:5E:00:53:2A", "", false},
		{"a section with no name of its own", "46:06:27:c5:bd:45", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := bluetoothName(path, tt.address)
			if got != tt.want || ok != tt.ok {
				t.Errorf("name = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestBluetoothNameUnescapesWhatTheConfigEscaped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bt_config.xml")
	config := `<N2 Tag="Remote">
        <N1 Tag="f8:5c:7d:39:15:31">
            <N2 Tag="Name" Type="string">Bob &amp; Ann&apos;s</N2>
        </N1>
    </N2>
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := bluetoothName(path, "F8:5C:7D:39:15:31")
	if !ok || got != "Bob & Ann's" {
		t.Errorf("name = %q, %v; want %q", got, ok, "Bob & Ann's")
	}
}

func TestBluetoothNameIsValidUTF8WhateverTheConfigHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bt_config.xml")
	config := "<N1 Tag=\"f8:5c:7d:39:15:31\">\n" +
		"<N2 Tag=\"Name\" Type=\"string\">Caf\xe9</N2>\n</N1>\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := bluetoothName(path, "F8:5C:7D:39:15:31")
	if !ok || got != "Caf\uFFFD" {
		t.Errorf("name = %q, %v; want %q: a proto3 string that is not UTF-8 makes Home "+
			"Assistant drop the connection", got, ok, "Caf\uFFFD")
	}
}

func TestBluetoothNameWithNoConfigFile(t *testing.T) {
	if _, ok := bluetoothName(filepath.Join(t.TempDir(), "gone.xml"), "F8:5C:7D:39:15:31"); ok {
		t.Error("a config file that is not there answered with a name")
	}
}

func TestBluetoothDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bt_config.xml")
	if err := os.WriteFile(path, []byte(btConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func(was dumpsys, wasPath string) {
		*btDump, btConfigPath = was, wasPath
	}(*btDump, btConfigPath)
	btConfigPath = path

	for _, tt := range []struct {
		name string
		dump string
		err  error
		want string
		ok   bool
	}{
		{"a speaker with a name in the config", a2dpDump, nil, "JBL Go 3", true},
		{"a speaker the config does not carry",
			"Profile: A2dpService\n  mCurrentDevice: 00:00:5E:00:53:2A\n", nil, "00:00:5E:00:53:2A", true},
		{"nothing is connected", "Profile: A2dpService\n  mCurrentDevice: null\n", nil, "", true},
		{"a dump that named no a2dp profile", "enabled: true\n", nil, "", false},
		{"a read that failed", a2dpDump, os.ErrPermission, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			btDump.command = func(context.Context) ([]byte, error) {
				return []byte(tt.dump), tt.err
			}
			got, ok := BluetoothDevice()
			if got != tt.want || ok != tt.ok {
				t.Errorf("device = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestAConnectedSpeakerNeverReadsAsNothingConnected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bt_config.xml")
	defer func(was dumpsys, wasPath string) {
		*btDump, btConfigPath = was, wasPath
	}(*btDump, btConfigPath)
	btConfigPath = path
	btDump.command = func(context.Context) ([]byte, error) { return []byte(a2dpDump), nil }

	for _, tt := range []struct {
		name   string
		config string
	}{
		{"a name that is empty",
			"<N1 Tag=\"f8:5c:7d:39:15:31\">\n<N2 Tag=\"Name\" Type=\"string\"></N2>\n</N1>\n"},
		{"a name that is only spaces",
			"<N1 Tag=\"f8:5c:7d:39:15:31\">\n<N2 Tag=\"Name\" Type=\"string\">   </N2>\n</N1>\n"},
		{"no name tag at all",
			"<N1 Tag=\"f8:5c:7d:39:15:31\">\n<N1 Tag=\"DevType\" Type=\"int\">1</N1>\n</N1>\n"},
		{"no entry for it at all", "<Bluedroid>\n</Bluedroid>\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tt.config), 0o600); err != nil {
				t.Fatal(err)
			}
			got, ok := BluetoothDevice()
			if !ok || strings.TrimSpace(got) == "" {
				t.Errorf("a connected speaker read as %q, %v; an empty reading is how this"+
					" sensor says nothing is connected", got, ok)
			}
		})
	}
}

func TestOneBluetoothReadCannotOutlastItsBudget(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no shell to fork with: %v", err)
	}
	defer func(was dumpsys) { *btDump = was }(*btDump)

	btDump.timeout, btDump.wait = 100*time.Millisecond, 400*time.Millisecond
	btDump.argv = []string{"sh", "-c", "sleep 30 & sleep 30"}

	start := time.Now()
	name, ok := BluetoothDevice()
	elapsed := time.Since(start)

	if ok || name != "" {
		t.Errorf("a read that never answered reported %q, %v", name, ok)
	}
	if elapsed < btDump.timeout {
		t.Fatalf("the read failed in %v, before the deadline it was supposed to hit; the command never ran", elapsed)
	}
	if limit := BluetoothReadBudget() + 100*time.Millisecond; elapsed > limit {
		t.Errorf("one read took %v against a budget of %v; the budget has to bound the whole call",
			elapsed, BluetoothReadBudget())
	}
}
