package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

// Call invokes a middleware method and returns its raw result.
//
// A dropped connection is retried once, because the appliance closes idle
// sockets and a caller should not see that. An authentication failure is never
// retried — see ErrAuthFailed.
func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := c.call(ctx, method, params)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		if errors.Is(err, ErrAuthFailed) || ctx.Err() != nil {
			return nil, err
		}
		if !errors.Is(err, ErrConnClosed) {
			return nil, err
		}
		// connection dropped: reconnect and try once more
	}
	return nil, lastErr
}

func (c *Client) call(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	if err := c.ensureConn(ctx); err != nil {
		return nil, err
	}
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer c.sem.Release(1)

	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return nil, ErrConnClosed
	}
	conn := c.conn
	c.nextID++
	id := c.nextID
	ch := make(chan *response, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	cleanup := func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}

	c.writeMu.Lock()
	err := conn.WriteJSON(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	c.writeMu.Unlock()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("%w: writing %s: %v", ErrConnClosed, method, err)
	}

	timer := time.NewTimer(callTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	case <-timer.C:
		cleanup()
		return nil, fmt.Errorf("%s: timed out after %s", method, callTimeout)
	case r, ok := <-ch:
		if !ok || r == nil {
			return nil, fmt.Errorf("%w during %s", ErrConnClosed, method)
		}
		if r.Error != nil {
			ce := &CallError{Method: method, Code: r.Error.Code}
			if r.Error.Data != nil {
				ce.ErrName = r.Error.Data.ErrName
				ce.Reason = r.Error.Data.Reason
			}
			if ce.Code == -32000 {
				return nil, fmt.Errorf("%w (%s)", ErrTooManyConcurrent, method)
			}
			return nil, ce
		}
		return r.Result, nil
	}
}

// CallJSON invokes a method and unmarshals its result into out.
func (c *Client) CallJSON(ctx context.Context, out any, method string, params ...any) error {
	raw, err := c.Call(ctx, method, params...)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}
