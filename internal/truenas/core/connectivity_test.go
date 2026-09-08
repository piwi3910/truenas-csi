package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/truenas"
	corefake "github.com/piwi3910/truenas-csi/internal/truenas/core/fake"
)

// TestConnectivityQueriesRefuseOnCORE. These three answer "is that node still
// holding the volume". CORE cannot answer them with the generic method router,
// and an empty answer is not a neutral one — it is the answer that authorises a
// fence. So each must refuse, loudly, without reaching the wire.
func TestConnectivityQueriesRefuseOnCORE(t *testing.T) {
	tests := []struct {
		name string
		call func(ctx context.Context, c *Client) error
	}{
		{"reporting.get_data", func(ctx context.Context, c *Client) error {
			_, err := c.ReportingGetData(ctx,
				[]truenas.ReportingQuery{{Name: truenas.GraphDisk, Identifier: "sda"}},
				time.Time{}, time.Time{})
			return err
		}},
		{"iscsi.global.sessions", func(ctx context.Context, c *Client) error {
			_, err := c.ISCSISessions(ctx)
			return err
		}},
		{"nfs clients", func(ctx context.Context, c *Client) error {
			_, err := c.NFSClients(ctx)
			return err
		}},
		{"iscsi.global.client_count", func(ctx context.Context, c *Client) error {
			_, err := c.ISCSIClientCount(ctx)
			return err
		}},
		{"nfs.client_count", func(ctx context.Context, c *Client) error {
			_, err := c.NFSClientCount(ctx)
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := corefake.Start(t, corefake.Options{})
			c := dialFake(t, s)

			err := tc.call(context.Background(), c)
			if !errors.Is(err, ErrNotSupported) {
				t.Fatalf("want ErrNotSupported, got %v", err)
			}
		})
	}
}
