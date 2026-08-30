package supervisor

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type ProcessReaper struct {
	mu       sync.Mutex
	handlers map[int]func(syscall.WaitStatus)
	pending  map[int]syscall.WaitStatus
}

func NewProcessReaper() *ProcessReaper {
	return &ProcessReaper{
		handlers: make(map[int]func(syscall.WaitStatus)),
		pending:  make(map[int]syscall.WaitStatus),
	}
}

func (r *ProcessReaper) Register(pid int, handler func(syscall.WaitStatus)) {
	if pid <= 0 || handler == nil {
		return
	}
	r.mu.Lock()
	if status, ok := r.pending[pid]; ok {
		delete(r.pending, pid)
		r.mu.Unlock()
		go handler(status)
		return
	}
	r.handlers[pid] = handler
	r.mu.Unlock()

	var status syscall.WaitStatus
	if reaped, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); err == nil && reaped == pid {
		r.handleExit(pid, status)
	}
}

func (r *ProcessReaper) Start() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	go func() {
		for range ch {
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if pid <= 0 {
					if err == syscall.EINTR {
						continue
					}
					break
				}
				r.handleExit(pid, status)
			}
		}
	}()
}

func (r *ProcessReaper) handleExit(pid int, status syscall.WaitStatus) {
	r.mu.Lock()
	handler := r.handlers[pid]
	if handler != nil {
		delete(r.handlers, pid)
	} else {
		r.pending[pid] = status
	}
	r.mu.Unlock()
	if handler != nil {
		go handler(status)
	}
}
