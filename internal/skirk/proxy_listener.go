package skirk

import (
	"context"
	"errors"
	"net"
	"time"
)

func serveProxyListener(ctx context.Context, listener net.Listener, handle func(context.Context, net.Conn)) error {
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var retryDelay time.Duration
	for {
		conn, err := listener.Accept()
		if err == nil {
			retryDelay = 0
			go handle(ctx, conn)
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		var temporary net.Error
		if !socketResourcesExhausted(err) && !(errors.As(err, &temporary) && temporary.Temporary()) {
			return err
		}
		// Match net/http's accept backoff while the OS recovers descriptors or buffers.
		if retryDelay == 0 {
			retryDelay = 5 * time.Millisecond
		} else {
			retryDelay = min(2*retryDelay, time.Second)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retryDelay):
		}
	}
}
