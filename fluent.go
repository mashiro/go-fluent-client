// Package fluent implements a client for the fluentd data logging daemon.
package fluent

import (
	"context"
	"time"

	pdebug "github.com/lestrrat/go-pdebug"
	"github.com/pkg/errors"
)

// New creates a new client. Options may be:
//
//   WithAddress: the address to connect to. default: 127.0.0.1:24224
//   WithBufferLimit: the maximum pending buffer size. default: 8MB
//   WithJSONMarshaler: use JSON serializer
//   WithMsgpackMarshaler: use MessagePack serializer (default)
//   WithNetwork: network type to use. default: tcp
//   WithTagPrefix: tag prefix to append to all tags
//   WithWriteThreshold: minimum number of bytes before starting to send to buffer to server
func New(options ...Option) (*Client, error) {
	m, err := newMinion(options...)
	if err != nil {
		return nil, err
	}

	var c Client
	ctx, cancel := context.WithCancel(context.Background())

	c.minionDone = m.done
	c.minionQueue = m.incoming
	c.minionCancel = cancel

	go m.runReader(ctx)
	go m.runWriter(ctx)

	return &c, nil
}

// Post posts the given structure after encoding it along with the given tag.
//
// An error is returned if the client has already been closed.
//
// If you would like to specify options to `Post()`, you may pass them at the end of
// the method. Currently you can use the following:
//
//   fluent.WithTimestamp: allows you to set arbitrary timestamp values
//   fluent.WithSyncAppend: allows you to verify if the append was successful
//
// If fluent.WithSyncAppend is provide and is true, the following errors
// may be returned:
//
//   1. If the current underlying pending buffer is is not large enough to
//      hold this new data, an error will be returned
//   2. If the marshaling into msgpack/json failed, it is returned
//
func (c *Client) Post(tag string, v interface{}, options ...Option) error {
	// Do not allow processing at all if we have closed
	c.muClosed.RLock()
	defer c.muClosed.RUnlock()

	if c.closed {
		return errors.New(`client has already been closed`)
	}

	var syncAppend bool
	var t int64
	for _, opt := range options {
		switch opt.Name() {
		case "timestamp":
			t = opt.Value().(time.Time).Unix()
		case "sync_append":
			syncAppend = opt.Value().(bool)
		}
	}
	if t == 0 {
		t = time.Now().Unix()
	}

	msg := getMessage()
	msg.Tag = tag
	msg.Time = t
	msg.Record = v

	// This has to be separate from msg.replyCh, b/c msg would be
	// put back to the pool
	var replyCh chan error
	if syncAppend {
		replyCh = make(chan error)
		msg.replyCh = replyCh
	}

	select {
	case <-c.minionDone:
		return errors.New("writer has been closed. Shutdown called?")
	case c.minionQueue <- msg:
	}

	if syncAppend {
		if pdebug.Enabled {
			pdebug.Printf("client: Post is waiting for return status")
		}
		select {
		case <-c.minionDone:
			return errors.New("writer has been closed. Shutdown called?")
		case e := <-replyCh:
			if pdebug.Enabled {
				pdebug.Printf("client: synchronous result received")
			}
			return e
		}
	}

	return nil
}

// Close closes the connection, but does not wait for the pending buffers
// to be flushed. If you want to make sure that background minion has properly
// exited, you should probably use the Shutdown() method
func (c *Client) Close() error {
	c.muClosed.Lock()
	c.closed = true
	c.muClosed.Unlock()

	c.minionCancel()
	return nil
}

// Shutdown closes the connection, and notifies the background worker to
// flush all existing buffers. This method will block until the
// background minion exits, or the provided context object is canceled.
func (c *Client) Shutdown(ctx context.Context) error {
	if pdebug.Enabled {
		pdebug.Printf("client: shutdown requested")
		defer pdebug.Printf("client: shutdown completed")
	}

	if ctx == nil {
		ctx = context.Background() // no cancel...
	}

	if err := c.Close(); err != nil {
		return errors.Wrap(err, `failed to close`)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.minionDone:
		return nil
	}
}
