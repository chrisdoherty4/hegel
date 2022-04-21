package signals

import (
	"context"
	"os"
)

func ContextCanceller(ctx context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	notifier := make(chan os.Signal, len(signals))
	ctx, cancel := context.WithCancel(ctx)
	go func() { <-notifier; cancel() }()
	return ctx, cancel
}
