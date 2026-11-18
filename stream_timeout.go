package resources

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

func normalizeDepError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var af *AccessFailure
	if errors.As(err, &af) {
		if af == nil || !af.valid() {
			return ErrUnavailable
		}
		return af
	}
	for _, known := range []error{ErrRateLimited, ErrInput, ErrUnauthenticated, ErrDenied, ErrNotFound, ErrConflict, ErrUnavailable, ErrCapacity} {
		if errors.Is(err, known) {
			return err
		}
	}
	return ErrUnavailable
}

type idleTimeoutWriter struct {
	w        io.Writer
	timeout  time.Duration
	timer    *time.Timer
	mu       sync.Mutex
	writeErr error
}

func (iw *idleTimeoutWriter) Write(p []byte) (int, error) {
	iw.mu.Lock()
	iw.timer.Reset(iw.timeout)
	iw.mu.Unlock()
	n, err := iw.w.Write(p)
	iw.mu.Lock()
	if err != nil {
		iw.writeErr = err
	}
	iw.timer.Reset(iw.timeout)
	iw.mu.Unlock()
	return n, err
}

func (s *Service) withStreamIdleTimeout(ctx context.Context, w io.Writer, run func(context.Context, io.Writer) error) error {
	streamCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	iw := &idleTimeoutWriter{w: w, timeout: s.cfg.OperationTimeout}
	iw.timer = time.AfterFunc(s.cfg.OperationTimeout, func() {
		cancel(context.DeadlineExceeded)
	})
	defer iw.timer.Stop()
	err := run(streamCtx, iw)
	if err == nil {
		return nil
	}
	iw.mu.Lock()
	wErr := iw.writeErr
	iw.mu.Unlock()
	if wErr != nil && errors.Is(err, wErr) {
		return err
	}
	return normalizeDepError(ctx, err)
}
