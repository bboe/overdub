package device

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOutputBluetooth(t *testing.T) {
	for _, tt := range []struct {
		output    string
		connected bool
		known     bool
	}{
		{"BT##", true, true},
		{"BT##JBL Go 3", true, true},
		{"SPEAKER##", false, true},
		{"AUX##", false, true},
		{"LINEOUT##", false, false},
		{"BT", false, false},
		{"", false, false},
	} {
		t.Run(tt.output, func(t *testing.T) {
			connected, known := outputBluetooth(tt.output)
			if connected != tt.connected || known != tt.known {
				t.Errorf("outputBluetooth(%q) = %v, %v; want %v, %v",
					tt.output, connected, known, tt.connected, tt.known)
			}
		})
	}
}

func TestTheRouteFollowsAudiodsEvents(t *testing.T) {
	resetVolume(t)
	if _, known := BluetoothRouted(); known {
		t.Fatal("a route nobody has read is known")
	}
	for _, tt := range []struct {
		line      string
		connected bool
	}{
		{`OutputDeviceChanged "BT##JBL Go 3" `, true},
		{`OutputDeviceChanged "SPEAKER##" `, false},
		{`OutputDeviceChanged "BT##Bob "Boom" C:\Speaker"`, true},
		{"\tOutputDeviceChanged \"SPEAKER##\"", false},
		{`OutputDeviceChanged "BT##"`, true},
		{`OutputDeviceChanged "AUX##" `, false},
		{`OutputDeviceChanged "SPEAKER##" `, false},
	} {
		volumeEvent(tt.line)
		if connected, known := BluetoothRouted(); connected != tt.connected || !known {
			t.Errorf("after %q the route is %v, %v; want %v, true", tt.line, connected, known, tt.connected)
		}
	}
	volumeEvent("OutputDeviceChanged BT##")
	if connected, _ := BluetoothRouted(); connected {
		t.Error("an event that is not quoted moved the route")
	}
	forgetVolume()
	if _, known := BluetoothRouted(); known {
		t.Error("a route nobody is watching is still known")
	}
}

const connectedList = `INFO[28706]: BT Client :: start
p_data (size:27) = 1B 00 00 00 00 00 00 00 00 00 00 62 74 63 6C 69
                   65 6E 74 00 00 00 00 00 00 00 00
INFO[28706]: BT Client :: opened session with 0xf2d3a000
INFO[28706]: queue_thread dispatched
>INFO[28706]: Got command connectedlist
INFO[28706]: Got 2 devices for getConnectedDevices
INFO[28706]: 00:00:5E:00:53:2A
INFO[28706]: F8:5C:7D:39:15:31
INFO[28706]: aceBT_freeDeviceList with status 0
`

func TestReadConnected(t *testing.T) {
	for _, tt := range []struct {
		name  string
		out   string
		want  []string
		fails bool
	}{
		{"two devices", connectedList, []string{"00:00:5e:00:53:2a", "f8:5c:7d:39:15:31"}, false},
		{"none", ">INFO[1]: Got command connectedlist\nINFO[1]: Got 0 devices for getConnectedDevices\n", nil, false},
		{"an address before the count is not one of them",
			"INFO[1]: F8:5C:7D:39:15:31\nINFO[1]: Got 0 devices for getConnectedDevices\n", nil, false},
		{"output that ends before the list does",
			"INFO[1]: Got 2 devices for getConnectedDevices\nINFO[1]: F8:5C:7D:39:15:31\n", nil, true},
		{"no count at all", "INFO[1]: BT Client :: start\n", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readConnected(strings.NewReader(tt.out))
			if (err != nil) != tt.fails || !slices.Equal(got, tt.want) {
				t.Errorf("readConnected = %q, %v; want %q, failing %v", got, err, tt.want, tt.fails)
			}
		})
	}
}

const btConfig = `[Info]
FileSource = Empty

[Adapter]
Address = 68:37:e9:5f:aa:76
Name = Echo Dot-3UX

[00:00:5e:00:53:2a]
Name = A Phone
Service = 0000110a-0000-1000-8000-00805f9b34fb 0000110c-0000-1000-8000-00805f9b34fb

[f8:5c:7d:39:15:31]
Timestamp = 1794756458
Name = JBL Go 3
Service = 0000110b-0000-1000-8000-00805f9b34fb 0000110e-0000-1000-8000-00805f9b34fb

[00:02:5b:00:15:10]
DevType = 1
`

func TestParseBTConfig(t *testing.T) {
	sections := parseBTConfig(btConfig)
	if got := sections["f8:5c:7d:39:15:31"]["Name"]; got != "JBL Go 3" {
		t.Errorf("the speaker's name is %q, want JBL Go 3", got)
	}
	if got := sections["00:02:5b:00:15:10"]["Name"]; got != "" {
		t.Errorf("a section with no name reads %q, a neighbour's", got)
	}
	if _, ok := sections["Adapter"]; ok {
		t.Error("the Dot's own section was read as a device")
	}
}

func fakeBTClient(t *testing.T, script string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no shell to fork with: %v", err)
	}
	was := btClientArgv
	t.Cleanup(func() { btClientArgv = was })
	btClientArgv = []string{"sh", "-c", script}
}

func TestBluetoothDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bt_config.conf")
	if err := os.WriteFile(path, []byte(btConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	wasPath := btConfigPath
	t.Cleanup(func() { btConfigPath = wasPath })
	btConfigPath = path

	list := func(addresses ...string) string {
		var b strings.Builder
		b.WriteString("read cmd; read quit; [ \"$quit\" = quit ] || exit 1; ")
		b.WriteString("echo 'INFO[1]: Got " + strconv.Itoa(len(addresses)) +
			" devices for getConnectedDevices'; ")
		for _, a := range addresses {
			b.WriteString("echo 'INFO[1]: " + a + "'; ")
		}
		return b.String()
	}
	for _, tt := range []struct {
		name   string
		output string
		client string
		want   string
		ok     bool
	}{
		{"a speaker with a name in the config", "BT##", list("F8:5C:7D:39:15:31"), "JBL Go 3", true},
		{"the sink, not a phone connected beside it", "BT##",
			list("00:00:5E:00:53:2A", "F8:5C:7D:39:15:31"), "JBL Go 3", true},
		{"a speaker the config does not carry", "BT##", list("00:00:5E:00:53:2B"), "00:00:5e:00:53:2b", true},
		{"a speaker the config has no name for", "BT##", list("00:02:5B:00:15:10"), "00:02:5b:00:15:10", true},
		{"two devices, neither a sink the config knows", "BT##",
			list("00:00:5E:00:53:2A", "00:02:5B:00:15:10"), "", false},
		{"nothing is routed there", "SPEAKER##", "exit 1", "", true},
		{"the jack took the route", "AUX##", "exit 1", "", true},
		{"a route audiod has not said", "", "exit 1", "", false},
		{"a client that fails", "BT##", "exit 1", "", false},
		{"a client that lists nothing while the route says bluetooth", "BT##", list(), "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetVolume(t)
			if tt.output != "" {
				volumeEvent(`OutputDeviceChanged "` + tt.output + `"`)
			}
			fakeBTClient(t, tt.client)
			got, ok := BluetoothDevice()
			if got != tt.want || ok != tt.ok {
				t.Errorf("device = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestTheClientIsAskedAndThenToldToQuit(t *testing.T) {
	log := filepath.Join(t.TempDir(), "stdin")
	fakeBTClient(t, "read a; echo \"$a\" > "+log+"; read b; echo \"$b\" >> "+log+
		"; echo 'INFO[1]: Got 0 devices for getConnectedDevices'")
	if _, err := connectedDevices(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "connectedlist\nquit\n" {
		t.Errorf("the client was sent %q, want connectedlist then quit: btclient buffers a pipe, so nothing is answered until it quits", got)
	}
}

func TestOneBluetoothReadCannotOutlastItsBudget(t *testing.T) {
	fakeBTClient(t, "sleep 30 & sleep 30")
	defer func(timeout, wait time.Duration) { btTimeout, btWait = timeout, wait }(btTimeout, btWait)
	btTimeout, btWait = 100*time.Millisecond, 400*time.Millisecond

	start := time.Now()
	_, err := connectedDevices()
	elapsed := time.Since(start)

	if err == nil {
		t.Error("a client that never answered gave a list")
	}
	if elapsed < btTimeout {
		t.Fatalf("the read failed in %v, before the deadline it was supposed to hit; the command never ran", elapsed)
	}
	if limit := BluetoothReadBudget() + 100*time.Millisecond; elapsed > limit {
		t.Errorf("one read took %v against a budget of %v; the budget has to bound the whole call",
			elapsed, BluetoothReadBudget())
	}
}

func TestTheSinkAndTheNameAreReadWhateverTheirCase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bt_config.conf")
	config := "[00:00:5e:00:53:2a]\nName = A Phone\n" +
		"[f8:5c:7d:39:15:31]\nName = Caf\xe9\nService = 0000110B-0000-1000-8000-00805F9B34FB\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	wasPath := btConfigPath
	t.Cleanup(func() { btConfigPath = wasPath })
	btConfigPath = path
	resetVolume(t)
	volumeEvent(`OutputDeviceChanged "BT##"`)
	fakeBTClient(t, "read cmd; read quit; echo 'INFO[1]: Got 2 devices for getConnectedDevices'; "+
		"echo 'INFO[1]: 00:00:5E:00:53:2A'; echo 'INFO[1]: F8:5C:7D:39:15:31'")
	got, ok := BluetoothDevice()
	if !ok || got != "Caf\uFFFD" {
		t.Errorf("device = %q, %v; want the sink, by an upper-case service, named as valid UTF-8", got, ok)
	}
}
