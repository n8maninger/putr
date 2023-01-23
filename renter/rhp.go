package renter

import (
	"context"
	"fmt"
	"net"

	"go.sia.tech/renterd/rhp/v2"
)

// dialTransport is a convenience function that connects to a host without
// locking a contract.
func dialTransport(ctx context.Context, hostIP string, hostKey rhp.PublicKey) (_ *rhp.Transport, err error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", hostIP)
	if err != nil {
		return nil, fmt.Errorf("failed to dial host: %w", err)
	}
	t, err := rhp.NewRenterTransport(conn, hostKey)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return t, nil
}
