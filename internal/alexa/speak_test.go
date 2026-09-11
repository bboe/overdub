package alexa

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func stubSpeak(t *testing.T, out string, err error) *[][]string {
	t.Helper()
	var calls [][]string
	was := speakCommand
	speakCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte(out), err
	}
	t.Cleanup(func() { speakCommand = was })
	return &calls
}

func argAfter(args []string, flag, name string) (string, bool) {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag && args[i+1] == name {
			return args[i+2], true
		}
	}
	return "", false
}

func TestSpeakHandsTheClipToTheSynthesizer(t *testing.T) {
	calls := stubSpeak(t, "", nil)
	const url = "http://192.168.2.5:8123/local/dot-tts/3-abc.mp3"

	if err := Speak(url); err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("Speak ran %d commands, want 1", len(*calls))
	}
	args := (*calls)[0]
	if !slices.Contains(args, speakAction) || !slices.Contains(args, speakService) {
		t.Errorf("the intent named %v, want the SpeechSynthesizer action and agent", args)
	}
	payload, ok := argAfter(args, "--es", "payload")
	if !ok || payload != `{"url":"`+url+`"}` {
		t.Errorf("payload = %q, want the url as json", payload)
	}
	array, ok := argAfter(args, "--esa", "payloads")
	if !ok || array != payload {
		t.Errorf("payloads = %q, want the same payload as the single extra: her handler reads "+
			"one of the two and which one is not ours to choose", array)
	}
}

func TestEveryDirectiveIsAddressedOnlyOnce(t *testing.T) {
	calls := stubSpeak(t, "", nil)
	for i := 0; i < 2; i++ {
		if err := Speak("http://example.invalid/a.mp3"); err != nil {
			t.Fatalf("Speak: %v", err)
		}
	}
	first, _ := argAfter((*calls)[0], "--es", "directiveId")
	second, _ := argAfter((*calls)[1], "--es", "directiveId")
	if first == "" || first == second {
		t.Errorf("two directives carried ids %q and %q, want two different ones", first, second)
	}
	firstSeq, _ := argAfter((*calls)[0], "--es", "sequenceId")
	secondSeq, _ := argAfter((*calls)[1], "--es", "sequenceId")
	if firstSeq == secondSeq {
		t.Errorf("two directives carried sequence %q twice", firstSeq)
	}
}

func TestAURLTheIntentCannotCarryIsNotSent(t *testing.T) {
	for _, tt := range []struct {
		name string
		url  string
	}{
		{"https, which is not the scheme measured", "https://example.invalid/a.mp3"},
		{"no scheme at all", "example.invalid/a.mp3"},
		{"a file on the device", "file:///data/local/tmp/a.mp3"},
		{"a comma, which ends an --esa element", "http://example.invalid/a,b.mp3"},
		{"a double quote, which ends the json", `http://example.invalid/a".mp3`},
		{"a backslash, which is an escape in both", `http://example.invalid/a\bc.mp3`},
		{"a trailing backslash, which escapes the closing quote", `http://example.invalid/a\`},
		{"nothing", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubSpeak(t, "", nil)
			if err := Speak(tt.url); err == nil {
				t.Error("the url was accepted")
			}
			if len(*calls) != 0 {
				t.Errorf("a url that cannot be carried still ran %d commands", len(*calls))
			}
		})
	}
}

func TestAFailureAmPrintsRatherThanReturnsIsStillAFailure(t *testing.T) {
	stubSpeak(t, "Starting service: Intent { act=... }\nError: Not found; no service started.\n", nil)
	err := Speak("http://example.invalid/a.mp3")
	if err == nil {
		t.Fatal("am printed an error, exited zero, and Speak reported success")
	}
	if !strings.Contains(err.Error(), "Not found") {
		t.Errorf("the error was %v, want the line am printed", err)
	}
}

func TestAnAmThatWillNotRunIsReported(t *testing.T) {
	stubSpeak(t, "", errors.New("fork/exec: no such file or directory"))
	if err := Speak("http://example.invalid/a.mp3"); err == nil {
		t.Error("a command that would not run reported success")
	}
}

func TestAmErrorReadsOnlyTheFailureLines(t *testing.T) {
	for _, tt := range []struct {
		name string
		out  string
		want string
	}{
		{"a clean start", "Starting service: Intent { act=x }\n", ""},
		{"an error after the announcement",
			"Starting service: Intent { act=x }\nError: Not found\n", "Error: Not found"},
		{"nothing at all", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := amError(tt.out); got != tt.want {
				t.Errorf("amError = %q, want %q", got, tt.want)
			}
		})
	}
}

func stubInstalled(t *testing.T, out string, err error) {
	t.Helper()
	was := installedCommand
	installedCommand = func(context.Context) ([]byte, error) { return []byte(out), err }
	t.Cleanup(func() { installedCommand = was })
}

func TestInstalledAsksThePackageManagerRatherThanTheFilesystem(t *testing.T) {
	for _, tt := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"a package with a path", "package:/system/priv-app/SpeechInteractionManager/SpeechInteractionManager.apk\n", nil, true},
		{"no package, and pm says so quietly", "", nil, false},
		{"no package, and pm exits non-zero", "", errors.New("exit status 1"), false},
		{"something that is not an answer", "Error: unknown command\n", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubInstalled(t, tt.out, tt.err)
			if got := Installed(); got != tt.want {
				t.Errorf("Installed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInstalledAsksAboutTheServiceTheIntentNames(t *testing.T) {
	if !strings.HasPrefix(speakService, Package+"/") {
		t.Errorf("the intent goes to %q and the check asks about %q; a check for a different "+
			"package answers a different question", speakService, Package)
	}
	if installedArgv[len(installedArgv)-1] != Package {
		t.Errorf("pm is asked about %q, want %q", installedArgv[len(installedArgv)-1], Package)
	}
	if installedArgv[0] != "/system/bin/sh" || installedArgv[1] != "/system/bin/pm" {
		t.Errorf("pm is run as %v, want it handed to the shell: /system/bin/pm carries no "+
			"shebang, so execve answers ENOEXEC and the check reports a package that is "+
			"installed as missing", installedArgv)
	}
	if speakArgv[0] == "/system/bin/pm" {
		t.Error("am is not pm: am does carry a shebang and is exec'd directly")
	}
}

func TestAPeersURLIsCutBeforeItReachesAnError(t *testing.T) {
	stubSpeak(t, "", nil)
	long := "ftp://" + strings.Repeat("x", 4096)

	err := Speak(long)
	if err == nil {
		t.Fatal("the url was accepted")
	}
	if len(err.Error()) > 256 {
		t.Errorf("one rejected url made a %d byte error, and that error is logged to /data: "+
			"a peer holding the key would be writing the log", len(err.Error()))
	}
}

func TestWhatAmPrintsIsCutToo(t *testing.T) {
	stubSpeak(t, "Error: "+strings.Repeat("y", 8192), nil)

	err := Speak("http://example.invalid/a.mp3")
	if err == nil {
		t.Fatal("am printed an error and Speak reported success")
	}
	if len(err.Error()) > 256 {
		t.Errorf("a stack trace from am made a %d byte error, and it is logged per request",
			len(err.Error()))
	}
}
