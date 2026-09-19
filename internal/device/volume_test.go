package device

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAVolumeIsSetOnTheStreamTheMusicPlaysOn(t *testing.T) {
	var got []string
	was := setVolumeCommand
	setVolumeCommand = func(_ context.Context, args []string) ([]byte, error) {
		got = args
		return []byte("Result: Parcel(00000000    '....')\n"), nil
	}
	defer func() { setVolumeCommand = was }()

	if err := SetMusicVolume(17); err != nil {
		t.Fatalf("SetMusicVolume: %v", err)
	}
	want := "/system/bin/service call audio 4 i32 3 i32 17 i32 0 s16 overdub"
	if line := strings.Join(got, " "); line != want {
		t.Errorf("the call was %q, want %q: the transaction number is this build's rather"+
			" than stock android's, and a wrong one writes something else in silence",
			line, want)
	}
}

func TestAVolumeTheServiceRefusedIsAnError(t *testing.T) {
	was := setVolumeCommand
	defer func() { setVolumeCommand = was }()

	for _, tt := range []struct {
		what string
		out  string
		err  error
	}{
		{"a transaction this build does not have", "Result: Parcel(NULL)\n", nil},
		{"an exception from the service", "Result: Parcel(fffffffd 00000010 'Bad caller')\n", nil},
		{"a service that could not be run", "", errors.New("exec: no such file")},
	} {
		setVolumeCommand = func(context.Context, []string) ([]byte, error) {
			return []byte(tt.out), tt.err
		}
		if err := SetMusicVolume(17); err == nil {
			t.Errorf("%s was taken as a level that landed, so nothing reports that the"+
				" level never moved", tt.what)
		}
	}
}

func TestAVolumeTheServiceTookIsNotAnError(t *testing.T) {
	was := setVolumeCommand
	defer func() { setVolumeCommand = was }()
	setVolumeCommand = func(context.Context, []string) ([]byte, error) {
		return []byte("Result: Parcel(00000000    '....')\n"), nil
	}
	if err := SetMusicVolume(0); err != nil {
		t.Errorf("a reply carrying no exception was read as a failure: %v", err)
	}
}
