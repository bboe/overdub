//go:build cgo

package opus

import (
	"math"
	"testing"
)

var turnEnvelope = []float64{ // dBFS per 100 ms
	-58.0, -58.8, -60.0, -58.4, -57.8, -59.8, -43.5, -39.2, -51.9, -50.6, -47.6, -54.7, -57.0, -43.7,
	-43.8, -53.2, -44.3, -45.0, -52.5, -45.9, -54.7, -57.5, -58.1, -58.5, -57.0, -58.2, -58.0,
}

func TestTheDevicesLibopusDecodesACapturedTurn(t *testing.T) {
	d, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	packets, _ := Split(turn(t))
	var pcm []int16
	for i, p := range packets {
		frame, err := d.Decode(p)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if len(frame) != FrameSamples {
			t.Fatalf("packet %d: %d samples, want %d", i, len(frame), FrameSamples)
		}
		pcm = append(pcm, frame...)
	}
	const window = Rate / 10
	for i, want := range turnEnvelope {
		var sum float64
		for _, s := range pcm[i*window : (i+1)*window] {
			sum += float64(s) * float64(s)
		}
		got := 20 * math.Log10(math.Sqrt(sum/window)/32768)
		if math.Abs(got-want) > 3 {
			t.Errorf("%.1f s: %.1f dBFS, want %.1f within 3 dB", float64(i)/10, got, want)
		}
	}
}

func TestADecoderRefusesWorkAfterClose(t *testing.T) {
	d, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if _, err := d.Decode(turn(t)[:PacketBytes]); err == nil {
		t.Fatal("a closed decoder decoded")
	}
}
