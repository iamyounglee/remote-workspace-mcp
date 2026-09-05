package audit

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIPKey 是客户端 IP 在 context 中的键类型。
type clientIPKey struct{}

// WithClientIP 返回携带客户端 IP 的 context，供请求链路上的审计埋点取用。
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFrom 从 context 取出客户端 IP；不存在时返回空字符串。
func ClientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

// IPResolver 解析审计使用的客户端 IP。
type IPResolver struct {
	// trusted 为受信任代理的地址段；为空表示不信任任何代理。
	trusted []netip.Prefix
}

// NewIPResolver 解析受信任代理条目，支持单个 IP 与 CIDR。
// 空列表表示不信任任何代理，此时 ClientIP 始终返回对端 IP。
// 任一条目非法即返回 error，由调用方终止启动。
func NewIPResolver(trusted []string) (*IPResolver, error) {
	prefixes := make([]netip.Prefix, 0, len(trusted))
	for _, entry := range trusted {
		p, err := parsePrefix(entry)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, p)
	}
	return &IPResolver{trusted: prefixes}, nil
}

// parsePrefix 将单个 IP 或 CIDR 条目解析为 netip.Prefix。
// 不含 "/" 的条目按其地址族展开为单地址前缀。
func parsePrefix(entry string) (netip.Prefix, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return netip.Prefix{}, fmt.Errorf("empty trusted proxy entry")
	}
	if strings.Contains(entry, "/") {
		p, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("invalid trusted proxy %q: %w", entry, err)
		}
		return p, nil
	}
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid trusted proxy %q: %w", entry, err)
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// ClientIP 返回审计使用的客户端 IP。
//
// 规则：仅当对端地址命中受信任代理时，才采信 X-Forwarded-For 中的客户端 IP；
// 否则一律返回对端 IP 并忽略该头，防止调用方伪造来源污染审计归因。
// 未配置受信任代理（trusted 为空）时，等价于始终返回对端 IP。
func (p *IPResolver) ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !p.trusts(addr) {
		return host
	}
	if fwd := firstForwardedFor(r.Header); fwd != "" {
		return fwd
	}
	return host
}

// trusts 判断给定地址是否位于受信任代理范围内。
func (p *IPResolver) trusts(addr netip.Addr) bool {
	for _, prefix := range p.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// firstForwardedFor 取 X-Forwarded-For 的首个条目（原始客户端）。
// 缺失或为空时返回空字符串。
func firstForwardedFor(h http.Header) string {
	raw := h.Get("X-Forwarded-For")
	if raw == "" {
		return ""
	}
	first, _, _ := strings.Cut(raw, ",")
	return strings.TrimSpace(first)
}
