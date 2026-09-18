package util

import "sync"

// switchSlot is the per-key gate state: a mutex serializing switches plus a
// monotonic sequence counting committed switches.
type switchSlot struct {
	gate    sync.Mutex
	waiting int
	seq     uint64
}

// SwitchGate serializes publish/obsolete switches that target the same logical
// key and records a monotonically increasing switch sequence per key.
//
// Usage per request:
//
//	token := gate.Enter(key)   // register and snapshot the sequence
//	gate.Await(key)            // wait until it is this request's turn
//	defer gate.Leave(key)
//	if gate.Sequence(key) != token { /* a concurrent switch won; reject */ }
//	... commit the switch in a database transaction ...
//	gate.Bump(key)             // invalidate concurrent waiters
//
// Enter registers and snapshots atomically, so a request that queues before
// the winner commits observes the winner's Bump and rejects itself; a request
// that arrives after the commit sees the new sequence and is treated as a
// fresh, sequential request. Operations on different keys never block each
// other.
type SwitchGate struct {
	mu    sync.Mutex
	slots map[string]*switchSlot
}

// NewSwitchGate creates an empty switch gate.
func NewSwitchGate() *SwitchGate {
	return &SwitchGate{slots: make(map[string]*switchSlot)}
}

// Enter registers interest in key, snapshots the committed-switch sequence and
// acquires the exclusive slot for that key. A caller blocks on acquisition
// until earlier holders Leave; its token still reflects the sequence captured
// at registration time, so a switch committed while queued is detectable via a
// Sequence comparison after Enter returns.
func (g *SwitchGate) Enter(key string) uint64 {
	g.mu.Lock()
	slot, exists := g.slots[key]
	if !exists {
		slot = &switchSlot{}
		g.slots[key] = slot
	}
	token := slot.seq
	slot.waiting++
	g.mu.Unlock()
	slot.gate.Lock()
	return token
}

// Leave releases the exclusive slot and, when nobody remains queued behind the
// caller, removes the registry entry to keep the gate bounded.
func (g *SwitchGate) Leave(key string) {
	g.mu.Lock()
	slot, exists := g.slots[key]
	if exists {
		slot.waiting--
		if slot.waiting <= 0 {
			delete(g.slots, key)
		}
	}
	g.mu.Unlock()
	if exists {
		slot.gate.Unlock()
	}
}

// Sequence returns the latest committed-switch sequence for key.
func (g *SwitchGate) Sequence(key string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if slot, exists := g.slots[key]; exists {
		return slot.seq
	}
	return 0
}

// Bump advances the committed-switch sequence. The caller must hold the slot
// (between Enter and Leave). It only takes the registry mutex briefly and never
// acquires the slot mutex, avoiding lock-ordering inversion with waiters.
func (g *SwitchGate) Bump(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if slot, exists := g.slots[key]; exists {
		slot.seq++
	}
}

// Waiting reports how many requests currently hold or queue for key. It is
// primarily used to make concurrency tests deterministic.
func (g *SwitchGate) Waiting(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if slot, exists := g.slots[key]; exists {
		return slot.waiting
	}
	return 0
}
