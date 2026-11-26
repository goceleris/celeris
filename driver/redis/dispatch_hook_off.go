//go:build !redis_delay_hook

package redis

// dispatchHook is a test hook called by redisState.dispatch between taking a
// request and writing its reply into it. In a normal build it is an empty
// function that the compiler inlines away; the redis_delay_hook build tag
// (dispatch_hook_on.go) replaces it with a delay, to model an event-loop
// worker that is descheduled at that point.
func dispatchHook() {}
