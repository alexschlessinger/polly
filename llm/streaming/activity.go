package streaming

import "context"

type activityObserverKey struct{}

// WithActivityObserver observes provider data without changing the watchdog's
// notifier. The observer runs on the provider goroutine and must not block.
func WithActivityObserver(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, activityObserverKey{}, fn)
}

func activityObserver(ctx context.Context) func() {
	fn, _ := ctx.Value(activityObserverKey{}).(func())
	return fn
}
