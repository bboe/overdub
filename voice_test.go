package main

import "testing"

func TestAnAnswerHeardOutsideMonitorIsDropped(t *testing.T) {
	noted := 0
	v := &voiceMode{mode: voicePass, note: func() { noted++ }}
	v.heard("It's 9:41.")
	if v.Reply() != "" || noted != 0 {
		t.Errorf("in pass through the reply became %q and was published %d times", v.Reply(), noted)
	}
	v.mode = voiceMonitor
	v.heard("It's 9:41.")
	if v.Reply() != "It's 9:41." || noted != 1 {
		t.Errorf("in monitor the reply is %q, published %d times", v.Reply(), noted)
	}
}
