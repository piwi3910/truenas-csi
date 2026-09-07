package truenas

import (
	"context"
	"encoding/json"
	"fmt"
)

// loginResult is the auth.login_ex response.
type loginResult struct {
	ResponseType string `json:"response_type"`
}

// login authenticates the current connection. The caller holds c.mu.
//
// Exactly one attempt is made, ever. auth.login_with_api_key is deliberately not
// used: it is the legacy call, and on 25.10 it fails even with a valid key.
func (c *Client) login(ctx context.Context) error {
	c.mu.Lock()
	conn := c.conn
	c.nextID++
	id := c.nextID
	ch := make(chan *response, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	drop := func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}

	params := []any{map[string]any{
		"mechanism": "API_KEY_PLAIN",
		"username":  c.backend.Username,
		"api_key":   c.backend.APIKey,
	}}
	c.writeMu.Lock()
	err := conn.WriteJSON(request{JSONRPC: "2.0", ID: id, Method: "auth.login_ex", Params: params})
	c.writeMu.Unlock()
	if err != nil {
		drop()
		return fmt.Errorf("%w: sending login: %v", ErrConnClosed, err)
	}

	select {
	case <-ctx.Done():
		drop()
		return ctx.Err()
	case r, ok := <-ch:
		if !ok || r == nil {
			return ErrConnClosed
		}
		if r.Error != nil {
			return fmt.Errorf("%w: %s", ErrAuthFailed, r.Error.Message)
		}
		var lr loginResult
		if err := json.Unmarshal(r.Result, &lr); err != nil {
			return fmt.Errorf("%w: undecodable login response", ErrAuthFailed)
		}
		if lr.ResponseType != "SUCCESS" {
			return fmt.Errorf("%w: response_type=%s (backend %s, user %s)",
				ErrAuthFailed, lr.ResponseType, c.backend.Name, c.backend.Username)
		}
		return nil
	}
}
