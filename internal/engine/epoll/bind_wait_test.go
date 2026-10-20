//go:build linux

package epoll

import (
	"fmt"
	"time"
)

// awaitBound waits up to d for e to bind its listeners and returns nil once
// Addr is set. If Listen returns first (an error before it binds, say) it does
// not wait out d: it returns that error, and puts the value back on done, which
// the caller's teardown still receives from. A test that fails here should
// report the error rather than a bare "did not bind".
func awaitBound(e *Engine, done chan error, d time.Duration) error {
	for dl := time.Now().Add(d); e.Addr() == nil; {
		select {
		case err := <-done:
			done <- err
			return fmt.Errorf("Listen returned before the engine bound: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(dl) {
			return fmt.Errorf("no address after %v", d)
		}
	}
	return nil
}
