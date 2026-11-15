package httpx

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// TrustedProxies is a set of networks whose X-Forwarded-For headers are
// believed. The zero value trusts nothing.
type TrustedProxies struct {
	nets []*net.IPNet
}

// ParseTrustedProxies parses CIDRs or bare IPs. Empty input yields no trusted
// proxies.
func ParseTrustedProxies(values []string) (TrustedProxies, error) {
	var tp TrustedProxies
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if strings.Contains(v, "/") {
			_, n, err := net.ParseCIDR(v)
			if err != nil {
				return TrustedProxies{}, fmt.Errorf("invalid trusted proxy %q: %w", raw, err)
			}
			tp.nets = append(tp.nets, n)
			continue
		}
		ip := net.ParseIP(strings.Trim(v, "[]"))
		if ip == nil {
			return TrustedProxies{}, fmt.Errorf("invalid trusted proxy IP %q", raw)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		mask := net.CIDRMask(bits, bits)
		tp.nets = append(tp.nets, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
	}
	return tp, nil
}

// Contains reports whether ip is in the trusted set.
func (tp TrustedProxies) Contains(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range tp.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Empty reports whether no networks are trusted.
func (tp TrustedProxies) Empty() bool { return len(tp.nets) == 0 }

// ClientIP returns the address used for logging and IP rate limiting. It only
// honors X-Forwarded-For when the immediate peer is trusted; otherwise an
// attacker could spoof the header and evade limits. The right-most untrusted
// address in the chain is returned.
func (tp TrustedProxies) ClientIP(r *http.Request) string {
	peer := RemoteIP(r.RemoteAddr)
	if peer == nil {
		return "unknown"
	}
	if !tp.Contains(peer) {
		return peer.String()
	}

	chain := forwardedChain(r)
	if len(chain) == 0 {
		return peer.String()
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if tp.Contains(chain[i]) {
			continue
		}
		return chain[i].String()
	}
	return chain[0].String()
}

// ClientIP is a convenience wrapper around TrustedProxies.
func ClientIP(r *http.Request, tp TrustedProxies) string { return tp.ClientIP(r) }

// RemoteIP extracts the IP from a RemoteAddr value (host:port, bare host).
func RemoteIP(remoteAddr string) net.IP {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return nil
	}
	if ip := parseIP(remoteAddr); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return parseIP(host)
	}
	return nil
}

func forwardedChain(r *http.Request) []net.IP {
	raw := r.Header.Values("X-Forwarded-For")
	if len(raw) == 0 {
		return nil
	}
	joined := strings.Join(raw, ",")
	if len(joined) > 4096 {
		joined = joined[:4096]
	}
	parts := strings.Split(joined, ",")
	if len(parts) > 32 {
		parts = parts[len(parts)-32:]
	}
	out := make([]net.IP, 0, len(parts))
	for _, part := range parts {
		if ip := parseIP(part); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

func parseIP(s string) net.IP {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return net.ParseIP(strings.Trim(host, "[]"))
	}
	return nil
}
