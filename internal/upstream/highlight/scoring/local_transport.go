package scoring

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var errLocalEndpointRejected = errors.New("local model endpoint rejected")

type loopbackResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func validateLocalBaseURL(raw string) error {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "?#") {
		return errors.New("invalid local base URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid local base URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("local base URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if !isLoopback(parsed.Hostname()) {
		return errors.New("local base URL host must be loopback")
	}
	return nil
}

func newLoopbackHTTPClient(timeout time.Duration, resolver loopbackResolver, dial dialContextFunc) *http.Client {
	return &http.Client{
		Transport: newLoopbackTransport(resolver, dial),
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newLoopbackTransport(resolver loopbackResolver, dial dialContextFunc) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A local client never inherits proxy settings or another provider's pool.
	transport.Proxy = nil
	transport.DialTLSContext = nil
	transport.DialContext = newLoopbackDialContext(resolver, dial)
	return transport
}

func newLoopbackDialContext(resolver loopbackResolver, dial dialContextFunc) dialContextFunc {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if dial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid local dial address: %w", err)
		}
		addresses, err := lookupLocalIPs(ctx, resolver, host)
		if err != nil {
			return nil, err
		}
		// Validate every result before the first dial. A single non-loopback or
		// nil answer rejects the whole request rather than filtering the list.
		for _, resolved := range addresses {
			if resolved.IP == nil || !resolved.IP.IsLoopback() {
				return nil, fmt.Errorf("%w: DNS returned a non-loopback address", errLocalEndpointRejected)
			}
		}

		var dialErrors []error
		for _, resolved := range addresses {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			connection, dialErr := dial(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if dialErr != nil {
				dialErrors = append(dialErrors, dialErr)
				continue
			}
			if connection == nil || !loopbackRemoteAddr(connectionRemoteAddr(connection)) {
				if connection != nil {
					_ = connection.Close()
				}
				return nil, fmt.Errorf("%w: actual peer is not loopback", errLocalEndpointRejected)
			}
			// net/http may reuse this connection without calling DialContext again.
			// Checking every write closes that bypass and also precedes TLS and
			// request-body writes on a newly established connection.
			return &loopbackConn{Conn: connection}, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(dialErrors) == 0 {
			return nil, errors.New("local endpoint has no dialable loopback address")
		}
		return nil, fmt.Errorf("local endpoint dial failed: %w", errors.Join(dialErrors...))
	}
}

func lookupLocalIPs(ctx context.Context, resolver loopbackResolver, host string) ([]net.IPAddr, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return nil, fmt.Errorf("%w: dial host is not loopback", errLocalEndpointRejected)
		}
		return []net.IPAddr{{IP: ip}}, nil
	}
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("local endpoint DNS resolution failed: %w", err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("%w: DNS returned no addresses", errLocalEndpointRejected)
	}
	return addresses, nil
}

func connectionRemoteAddr(connection net.Conn) net.Addr {
	if connection == nil {
		return nil
	}
	return connection.RemoteAddr()
}

func loopbackRemoteAddr(address net.Addr) bool {
	if address == nil {
		return false
	}
	if tcpAddress, ok := address.(*net.TCPAddr); ok {
		return tcpAddress != nil && tcpAddress.IP != nil && tcpAddress.IP.IsLoopback()
	}
	host, _, err := net.SplitHostPort(address.String())
	return err == nil && isLoopback(host) && net.ParseIP(host) != nil
}

type loopbackConn struct {
	net.Conn
}

func (c *loopbackConn) Write(buffer []byte) (int, error) {
	if !loopbackRemoteAddr(c.RemoteAddr()) {
		return 0, fmt.Errorf("%w: actual peer changed to a non-loopback address", errLocalEndpointRejected)
	}
	return c.Conn.Write(buffer)
}
