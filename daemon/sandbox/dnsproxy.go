// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// dnsproxy.go is the plan's "tiny DNS proxy": a per-sandbox resolver
// bound to the sandbox's gateway IP (the veth host end). It (1) answers
// ONLY for the sandbox's allowlisted FQDNs — disallowed names get
// REFUSED, so a compromised workload can't even learn a forbidden host's
// address — and (2) on an allowed answer, pushes the resolved IPs into
// that sandbox's nft allow set so the subsequent connection passes. This
// resolves the earlier real-kernel finding (the host /etc/resolv.conf
// systemd symlink is unbindable and 127.0.0.53 is unreachable from the
// netns): the sandbox's resolv.conf points at the gateway, which is
// directly connected over the veth.
//
// Minimal hand-rolled DNS (UDP, A records) — no new dependency, in
// keeping with the daemon's minimal-deps rule. Upstream resolution uses
// the host resolver via net.Resolver.

// WildcardFQDNs returns the "*."-prefixed patterns in the policy.
func WildcardFQDNs(pol EgressPolicy) []string {
	var w []string
	for _, f := range pol.AllowFQDNs {
		if strings.HasPrefix(f, "*.") {
			w = append(w, f)
		}
	}
	return w
}

// ResolveFQDNs eagerly resolves the concrete (non-wildcard) names to a
// sorted, de-duplicated IPv4 list. Used for static pre-population; the
// live path is the DNSProxy.
func ResolveFQDNs(ctx context.Context, fqdns []string) []string {
	r := &net.Resolver{}
	seen := map[string]bool{}
	var ips []string
	for _, f := range fqdns {
		if strings.HasPrefix(f, "*.") || f == "" {
			continue
		}
		addrs, err := r.LookupIP(ctx, "ip4", f)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if s := a.String(); !seen[s] {
				seen[s] = true
				ips = append(ips, s)
			}
		}
	}
	sort.Strings(ips)
	return ips
}

// allowedFQDN reports whether name (lowercased, no trailing dot) is
// permitted by the policy, honoring "*.suffix" wildcards.
func allowedFQDN(name string, pol EgressPolicy) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for _, f := range pol.AllowFQDNs {
		f = strings.ToLower(f)
		if strings.HasPrefix(f, "*.") {
			if strings.HasSuffix(name, f[1:]) || name == f[2:] {
				return true
			}
			continue
		}
		if name == f {
			return true
		}
	}
	return false
}

// DNSProxy is a per-sandbox filtering resolver.
type DNSProxy struct {
	id   string
	pol  EgressPolicy
	mode EgressMode
	conn *net.UDPConn
	res  *net.Resolver
	mu   sync.Mutex
	seen map[string]bool // dedupe IPs already pushed to the nft set
	obs  map[string]bool // distinct FQDNs observed OUTSIDE the allowlist
}

// StartDNSProxy binds udp <gatewayIP>:53 and serves until ctx is done.
// Returns once listening (serving continues in a goroutine).
//
// mode is the egress mode (Phase 3). It changes how a name NOT on the
// allowlist is handled:
//   - EgressEnforce: REFUSED (a compromised workload can't even learn a
//     forbidden host's address — defense in depth).
//   - EgressAudit: resolved + the IPs pushed to the nft set (whose audit
//     rule log+accepts) so the connection is NOT blocked, AND the name
//     is recorded as an out-of-policy observation. This is what makes
//     audit mode actually non-blocking end-to-end and is the data the
//     Phase 3 discovery-mode manifest proposal is built from. An
//     unmanifested (discovery) system runs with an empty allowlist in
//     audit mode, so every name it reaches is observed.
func StartDNSProxy(ctx context.Context, id, gatewayIP string, pol EgressPolicy, mode EgressMode) (*DNSProxy, error) {
	if !sandboxIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid sandbox id %q", id)
	}
	addr := &net.UDPAddr{IP: net.ParseIP(gatewayIP), Port: 53}
	c, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("dns listen %s:53: %w", gatewayIP, err)
	}
	p := &DNSProxy{id: id, pol: pol, mode: mode, conn: c, res: &net.Resolver{},
		seen: map[string]bool{}, obs: map[string]bool{}}
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	go p.serve(ctx)
	return p, nil
}

// ObservedFQDNs returns the distinct names this sandbox reached that
// were NOT on its allowlist (audit/discovery mode only). Sorted. This
// is the basis of the Phase 3 manifest proposal.
func (p *DNSProxy) ObservedFQDNs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.obs))
	for n := range p.obs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (p *DNSProxy) serve(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		n, raddr, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			return // closed
		}
		req := append([]byte(nil), buf[:n]...)
		go p.handle(ctx, req, raddr)
	}
}

func (p *DNSProxy) handle(ctx context.Context, req []byte, raddr *net.UDPAddr) {
	name, qtype, ok := parseQuestion(req)
	if !ok {
		return
	}
	// A name outside the allowlist: in enforce mode it is REFUSED
	// outright (defense in depth — the workload can't even learn the
	// address). In audit/discovery mode it is recorded as an
	// out-of-policy observation and then resolved normally, so audit
	// mode does NOT break the connection (the nft audit rule
	// log+accepts) and the observation feeds the manifest proposal.
	if !allowedFQDN(name, p.pol) {
		if p.mode == EgressEnforce {
			p.conn.WriteToUDP(buildResponse(req, name, qtype, nil, rcodeRefused), raddr)
			return
		}
		p.mu.Lock()
		if !p.obs[name] {
			p.obs[name] = true
			log.Printf("egress-observe sandbox=%s fqdn=%s (audit: out-of-policy, allowed+logged)", p.id, name)
		}
		p.mu.Unlock()
	}
	if qtype != 1 {
		// allowed name but non-A query: empty NOERROR.
		p.conn.WriteToUDP(buildResponse(req, name, qtype, nil, rcodeNoError), raddr)
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ipsv4 := []net.IP{}
	if addrs, err := p.res.LookupIP(rctx, "ip4", name); err == nil {
		ipsv4 = addrs
	}
	if len(ipsv4) == 0 {
		p.conn.WriteToUDP(buildResponse(req, name, qtype, nil, rcodeNoError), raddr)
		return
	}
	// Push to the nft allow set so the connection that follows passes.
	var fresh []string
	p.mu.Lock()
	for _, ip := range ipsv4 {
		s := ip.String()
		if !p.seen[s] {
			p.seen[s] = true
			fresh = append(fresh, s)
		}
	}
	p.mu.Unlock()
	if len(fresh) > 0 {
		sort.Strings(fresh)
		_ = PopulateAllowSet(p.id, fresh)
	}
	p.conn.WriteToUDP(buildResponse(req, name, qtype, ipsv4, rcodeNoError), raddr)
}

// --- minimal DNS wire format (A records, no compression on answer) ---

const (
	rcodeNoError = 0
	rcodeRefused = 5
)

// parseQuestion extracts the first question's name + qtype from a DNS
// query message. Returns ok=false on malformed input.
func parseQuestion(msg []byte) (name string, qtype uint16, ok bool) {
	if len(msg) < 12 {
		return "", 0, false
	}
	qd := binary.BigEndian.Uint16(msg[4:6])
	if qd < 1 {
		return "", 0, false
	}
	off := 12
	var labels []string
	for off < len(msg) {
		l := int(msg[off])
		off++
		if l == 0 {
			break
		}
		if l&0xC0 != 0 || off+l > len(msg) { // no compression in questions
			return "", 0, false
		}
		labels = append(labels, string(msg[off:off+l]))
		off += l
	}
	if off+4 > len(msg) {
		return "", 0, false
	}
	qtype = binary.BigEndian.Uint16(msg[off : off+2])
	return strings.Join(labels, "."), qtype, true
}

// buildResponse crafts a response echoing the query's question, with
// the given rcode and (for A answers) one A RR per ip.
func buildResponse(req []byte, name string, qtype uint16, ips []net.IP, rcode byte) []byte {
	out := make([]byte, 0, 512)
	// Header: copy ID, set QR=1, RD copied, RA=1, rcode.
	out = append(out, req[0], req[1])
	flags1 := byte(0x80) | (req[2] & 0x01)      // QR=1 + RD bit
	out = append(out, flags1, 0x80|rcode)       // RA=1 | rcode
	out = binary.BigEndian.AppendUint16(out, 1) // QDCOUNT
	an := uint16(0)
	for range ips {
		an++
	}
	out = binary.BigEndian.AppendUint16(out, an) // ANCOUNT
	out = binary.BigEndian.AppendUint16(out, 0)  // NSCOUNT
	out = binary.BigEndian.AppendUint16(out, 0)  // ARCOUNT
	// Question.
	qstart := len(out)
	for _, lab := range strings.Split(name, ".") {
		if lab == "" {
			continue
		}
		out = append(out, byte(len(lab)))
		out = append(out, lab...)
	}
	out = append(out, 0)
	out = binary.BigEndian.AppendUint16(out, qtype)
	out = binary.BigEndian.AppendUint16(out, 1) // class IN
	qnameLen := len(out) - qstart
	// Answers: a pointer to the question name (0xC00C) keeps it small.
	for _, ip := range ips {
		v4 := ip.To4()
		if v4 == nil {
			continue
		}
		out = append(out, 0xC0, 0x0C)                // name ptr -> offset 12 (question)
		out = binary.BigEndian.AppendUint16(out, 1)  // TYPE A
		out = binary.BigEndian.AppendUint16(out, 1)  // CLASS IN
		out = binary.BigEndian.AppendUint32(out, 30) // TTL 30s (short)
		out = binary.BigEndian.AppendUint16(out, 4)  // RDLENGTH
		out = append(out, v4...)
	}
	_ = qnameLen
	return out
}

// WriteResolvConf writes a sandbox resolv.conf pointing at the gateway
// DNS proxy and returns its host path. Bound into the sandbox by the
// spawner (SpawnOpts.ResolvConf) at ResolvConfDest().
func WriteResolvConf(id, gatewayIP string) (string, error) {
	path := "/run/vc-resolv-" + id + ".conf"
	body := "nameserver " + gatewayIP + "\noptions timeout:2 attempts:2 ndots:0\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		return "", err
	}
	return path, nil
}

// ResolvConfDest returns where the resolv.conf must be bound INSIDE the
// sandbox. Ubuntu's /etc/resolv.conf is a symlink (e.g. ->
// ../run/systemd/resolve/stub-resolv.conf); bwrap can't overlay the
// symlink itself, so we bind to its resolved real target (which bwrap
// creates parent dirs for on the tmpfs root). Falls back to
// /etc/resolv.conf when it is a regular file.
func ResolvConfDest() string {
	const def = "/etc/resolv.conf"
	fi, err := os.Lstat(def)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return def
	}
	tgt, err := os.Readlink(def)
	if err != nil {
		return def
	}
	if !strings.HasPrefix(tgt, "/") {
		// relative to /etc
		tgt = "/etc/" + tgt
	}
	// Clean ../ segments.
	parts := []string{}
	for _, seg := range strings.Split(tgt, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, seg)
		}
	}
	return "/" + strings.Join(parts, "/")
}
