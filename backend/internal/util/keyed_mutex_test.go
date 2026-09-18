package util

import (
	"runtime"
	"sync"
	"testing"
)

// Two queued Enter calls: the first holder commits and Bumps; the second Enter
// then returns with the pre-commit token while Sequence already advanced, so it
// must detect that a concurrent switch won.
func TestSwitchGateSerializesAndInvalidatesConcurrentWaiter(t *testing.T) {
	gate := NewSwitchGate()
	const key = "vessel/recipe"
	holderAcquired := make(chan struct{})
	proceed := make(chan struct{})
	waiterEntered := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		token := gate.Enter(key)
		defer gate.Leave(key)
		close(holderAcquired)
		<-proceed
		if gate.Sequence(key) != token {
			t.Error("first request observed an unexpected sequence change")
			return
		}
		gate.Bump(key)
	}()
	go func() {
		defer wg.Done()
		<-holderAcquired
		close(waiterEntered)
		// Enter registers with waiting++ then blocks on the slot; it returns
		// only once the first holder Leaves after Bump.
		token := gate.Enter(key)
		defer gate.Leave(key)
		if gate.Sequence(key) == token {
			t.Error("concurrent waiter must observe the first commit's sequence bump")
		}
	}()
	<-waiterEntered
	// Give the waiter a moment to actually queue on the slot mutex.
	spinWaiting(t, gate, key, 2)
	close(proceed)
	wg.Wait()
	if gate.Waiting(key) != 0 {
		t.Fatal("gate slot should be cleaned up after all requests leave")
	}
}

func TestSwitchGateIsIndependentAcrossKeys(t *testing.T) {
	gate := NewSwitchGate()
	a := gate.Enter("a")
	defer gate.Leave("a")
	b := gate.Enter("b")
	defer gate.Leave("b")
	gate.Bump("a")
	if gate.Sequence("b") != b {
		t.Fatal("switch on key a must not advance sequence for key b")
	}
	if gate.Sequence("a") == a {
		t.Fatal("sequence for key a should have advanced")
	}
}

func spinWaiting(t *testing.T, gate *SwitchGate, key string, want int) {
	t.Helper()
	for i := 0; i < 1000 && gate.Waiting(key) < want; i++ {
		runtime.Gosched()
	}
	if gate.Waiting(key) < want {
		t.Fatalf("waiting=%d want at least %d queued", gate.Waiting(key), want)
	}
}
