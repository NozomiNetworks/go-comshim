package comshim

import (
	"runtime"
	"sync"

	"github.com/go-ole/go-ole"
)

// Shim provides control of a thread-locked goroutine that has been initialized
// for use with a mulithreaded component object model apartment. This is used
// to ensure that at least one thread within a process maintains an
// initialized connection to COM, and thus prevents COM resources from being
// unloaded from that process.
//
// Control is implemented through the use of a counter similar to a waitgroup.
// As long as the counter is greater than zero then the goroutine will remain
// in a blocked condition with its COM connection intact.
type Shim struct {
	startAccess  sync.RWMutex
	running      bool
	cond         sync.Cond
	signalAccess sync.RWMutex
	c            Counter // An atomic counter
	wg           sync.WaitGroup
}

// New returns a new shim for keeping component object model resources allocated
// within a process.
func New() *Shim {
	shim := new(Shim)
	shim.cond.L = &shim.signalAccess
	shim.wg = sync.WaitGroup{}
	return shim
}

// TryAdd adds delta, which may be negative, to the counter for the shim. As long
// as the counter is greater than zero, at least one thread is guaranteed to be
// initialized for mutli-threaded COM access.
//
// If the counter becomes zero, the shim is released and COM resources may be
// released if there are no other threads that are still initialized.
//
// If the counter goes negative, TryAdd panics.
//
// If the shim cannot be created for some reason, TryAdd returns an error.
func (s *Shim) TryAdd(delta int) error {
	s.startAccess.Lock()
	defer s.startAccess.Unlock()
	s.add(delta)
	if s.running {
		return nil //already loaded
	}

	// The shim wasn't running; only change the running state within a write lock
	if s.running {
		// The shim was started between the read lock and the write lock
		return nil
	}

	if err := s.run(); err != nil {
		return err
	}

	s.running = true
	return nil
}

// Add adds delta, which may be negative, to the counter for the shim. As long
// as the counter is greater than zero, at least one thread is guaranteed to be
// initialized for mutli-threaded COM access.
//
// If the counter becomes zero, the shim is released and COM resources may be
// released if there are no other threads that are still initialized.
//
// If the counter goes negative, Add panics.
//
// If the shim cannot be created for some reason, Add panics.
func (s *Shim) Add(delta int) {
	if err := s.TryAdd(delta); err != nil {
		panic(err)
	}
}

// Done decrements the counter for the shim.
func (s *Shim) Done() {
	s.add(-1)
}

func (s *Shim) add(delta int) {
	s.signalAccess.Lock()
	defer s.signalAccess.Unlock()
	value := s.c.Add(int64(delta))
	if value == 0 {
		s.cond.Broadcast()
	}
	if value < 0 {
		panic(ErrNegativeCounter)
	}
}

func (s *Shim) run() error {
	init := make(chan error)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
			switch err.(*ole.OleError).Code() {
			case 0x00000001: // S_FALSE
				// Some other goroutine called CoInitialize on this thread
				// before we ended up with it. This probably means the other
				// caller failed to lock the OS thread or failed to call
				// CoUninitialize.

				// We still decrement this thread's initialization counter by
				// calling CoUninitialize here, as recommended by the docs.
				ole.CoUninitialize()

				// Send an error so that shim.Add panics
				init <- ErrAlreadyInitialized
			default:
				init <- err
			}
			close(init)
			return
		}

		close(init)

		s.signalAccess.Lock()
		for s.c.Value() > 0 {
			s.cond.Wait()
		}
		s.running = false
		ole.CoUninitialize()
		s.signalAccess.Unlock()
	}()

	return <-init
}

func (s *Shim) WaitDone() {
	s.startAccess.Lock()
	defer s.startAccess.Unlock()
	s.wg.Wait()
}
