package device

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

func Dialer(timeout time.Duration) *net.Dialer {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var servers []string
			for _, prop := range []string{"net.dns1", "net.dns2"} {
				if s := strings.TrimSpace(Getprop(prop)); s != "" {
					servers = append(servers, net.JoinHostPort(s, "53"))
				}
			}
			if len(servers) == 0 {
				return nil, fmt.Errorf("no net.dns1/net.dns2 to resolve with")
			}
			var d net.Dialer
			var err error
			for _, s := range servers {
				var conn net.Conn
				if conn, err = d.DialContext(ctx, network, s); err == nil {
					return conn, nil
				}
			}
			return nil, err
		},
	}
	return &net.Dialer{Timeout: timeout, Resolver: resolver}
}
