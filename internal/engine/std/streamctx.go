package std

import (
	"context"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// BindStreamCancel makes s's context end when ctx does, so a handler that
// waits on c.Context() is told that an HTTP/2 client reset the stream or went
// away (celeris#949): net/http, and the HTTP/2 server behind the std engine's
// h2c front end, cancel a request's context for both. s must be an HTTP/2
// stream (stream.NewStream): an HTTP/1 stream's context is
// context.Background() whatever is cancelled (as it is on the native
// engines), so binding one does nothing.
//
// The returned unbind must be called before s is released. A stream.Stream is
// pooled and its next use belongs to another request, so a cancel that
// arrives after Release would cancel that request. context.AfterFunc runs its
// function in a goroutine of its own, which unbind therefore waits for if it
// has already started. The wait takes no lock and is for a call of
// Stream.Cancel, which does not block. A second call of unbind returns at
// once.
//
// A context that has ended already cancels s before BindStreamCancel
// returns, without a goroutine; one that can never end (context.Background)
// binds nothing and allocates nothing.
func BindStreamCancel(ctx context.Context, s *stream.Stream) (unbind func()) {
	if ctx.Done() == nil {
		return nopUnbind
	}
	if ctx.Err() != nil {
		s.Cancel()
		return nopUnbind
	}
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		s.Cancel()
		close(fired)
	})
	return func() {
		if stop() {
			// The function will never run: close fired for a second call,
			// which finds stop() false and waits on it.
			close(fired)
			return
		}
		<-fired // the cancel has started (or finished): let it finish
	}
}

func nopUnbind() {}
