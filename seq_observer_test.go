package main

import (
	"testing"
	"time"
)

var (
	seqKey1 = sequenceId("a")
	seqKey2 = sequenceId("b")
)

func TestSeqObsNoReg(t *testing.T) {
	s := newSequencePubSub()
	s.Pub(seqKey1, 845)
	s.Stop()
}

func TestSeqObsStop(t *testing.T) {
	s := newSequencePubSub()
	ch := s.Sub(seqKey1, 4)
	go s.Stop()
	val, ok := <-ch
	if ok {
		t.Errorf("Expected close on stop, got %v", val)
	}
}

func assertNoSeqMessage(t *testing.T, chs ...<-chan int64) {
	for _, ch := range chs {
		select {
		case got := <-ch:
			t.Fatalf("Expected no message, got %v", got)
		case <-time.After(time.Millisecond * 3):
			// OK
		}
	}
}

func tseqmesg(t *testing.T, ch <-chan int64) int64 {
	select {
	case got := <-ch:
		return got
	case <-time.After(time.Millisecond * 3):
		t.Fatalf("Found no message where one was expected")
	}
	panic("unreachable")
}

func TestSeqObsPub(t *testing.T) {
	s := newSequencePubSub()
	defer s.Stop()
	ch1a := s.Sub(seqKey1, 4)
	ch1b := s.Sub(seqKey1, 2)
	ch1c := s.Sub(seqKey1, 10)

	ch2a := s.Sub(seqKey2, 3)

	s.Pub(seqKey1, 3)
	if got := tseqmesg(t, ch1b); got != 3 {
		t.Fatalf("Expected 3, got %v", got)
	}
	assertNoSeqMessage(t, ch1a, ch1c, ch2a)

	s.Pub(seqKey1, 15)
	for _, ch := range []<-chan int64{ch1a, ch1c} {
		if got := tseqmesg(t, ch); got != 15 {
			t.Fatalf("Expected 15, got %v", got)
		}
	}
	assertNoSeqMessage(t, ch2a)
}

func TestSeqLateRegistration(t *testing.T) {
	// "I'll be there in five minutes."
	// Five hours later: "I'll be there in five minutes."
	s := newSequencePubSub()
	defer s.Stop()
	ch1 := s.Sub(seqKey1, 2)

	s.Pub(seqKey1, 3)
	if got := tseqmesg(t, ch1); got != 3 {
		t.Fatalf("Expected 3, got %v", got)
	}

	// This should fire instantly
	ch2 := s.Sub(seqKey1, 3)
	if got := tseqmesg(t, ch2); got != 3 {
		t.Fatalf("Expected 3, got %v", got)
	}
}

func TestSeqRewind(t *testing.T) {
	// Going backwards shouldn't.
	s := newSequencePubSub()
	defer s.Stop()
	ch1 := s.Sub(seqKey1, 2)

	s.Pub(seqKey1, 3)
	if got := tseqmesg(t, ch1); got != 3 {
		t.Fatalf("Expected 3, got %v", got)
	}

	s.Pub(seqKey1, 2)

	ch2 := s.Sub(seqKey1, 3)
	if got := tseqmesg(t, ch2); got != 3 {
		t.Fatalf("Expected 3, got %v", got)
	}
}

func TestSeqDelete(t *testing.T) {
	s := newSequencePubSub()
	defer s.Stop()
	ch1 := s.Sub(seqKey1, 2)

	s.Pub(seqKey1, 3)
	if got := tseqmesg(t, ch1); got != 3 {
		t.Fatalf("Expected 3, got %v", got)
	}

	ch2 := s.Sub(seqKey1, 5)

	s.Delete(seqKey1)

	// This should fire immediately:
	select {
	case got, ok := <-ch2:
		if ok {
			t.Fatalf("Expected closed channel, got %v", got)
		}
	case <-time.After(3 * time.Millisecond):
		t.Fatalf("Timed out waiting for close")
	}

	// This should not fire
	ch3 := s.Sub(seqKey1, 3)
	assertNoSeqMessage(t, ch3)
}

func Testi64max(t *testing.T) {
	tests := []struct{ a, b, exp int64 }{
		{1, 2, 2},
		{2, 1, 2},
		{1, 1, 1},
	}

	for _, test := range tests {
		got := i64max(test.a, test.b)
		if got != test.exp {
			t.Errorf("Expected int64max(%v, %v) == %v, got %v",
				test.a, test.b, test.exp, got)
		}
	}
}
