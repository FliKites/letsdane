package resolver

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/miekg/dns"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientResolver implements Resolver and caches queries.
type AD struct {
	rrCache map[uint16]*cache
	client  *DNSClient

	exchangeFunc func(m *dns.Msg, client *DNSClient) (r *dns.Msg, rtt time.Duration, err error)
	Verify       func(m *dns.Msg) error
}

type DNSClient struct {
	d       *dns.Client
	addr string
}

const (
	maxAttempts = 3
	minTTL      = 10 * time.Second
	maxTTL      = 3 * time.Hour
	// max cache len for each rr type
	maxCache = 5000
)

func parseSimpleAddr(server string) (string, error) {
	_, _, err := net.SplitHostPort(server)
	if err == nil {
		return server, nil
	}

	return net.JoinHostPort(server, "53"), nil
}

func parseAddress(server string) (string, string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", "", fmt.Errorf("couldn't parse server address: %v", err)
	}

	var p, defaultPort string
	host := u.Host

	switch u.Scheme {
	case "udp":
		defaultPort = "53"
		p = ""
	case "tcp":
		p = u.Scheme
		defaultPort = "53"
	case "tls":
		p = "tcp-tls"
		defaultPort = "853"
	case "https":
		p = u.Scheme
		host = u.Scheme + "://" + u.Host
	default:
		return "", "", fmt.Errorf("unsupported scheme %s", u.Scheme)
	}

	_, _, err = net.SplitHostPort(u.Host)
	if err != nil && u.Scheme != "https" {
		return net.JoinHostPort(host, defaultPort), p, nil
	}

	return host, p, nil

}

// NewAD creates a new ad resolver
func NewAD(server string) (*AD, error) {
	addr, proto, err := parseAddress(server)

	if err != nil {
		addr, err = parseSimpleAddr(server)

		if err != nil {
			return nil, err
		}
		proto = "udp"
	}

	client := &DNSClient{}
	client.addr = addr

	client.d = new(dns.Client)
	client.d.Net = proto

	rrCache := make(map[uint16]*cache)
	rrCache[dns.TypeA] = newCache(maxCache)
	rrCache[dns.TypeAAAA] = newCache(maxCache)
	rrCache[dns.TypeTLSA] = newCache(maxCache)

	return &AD{
		rrCache:      rrCache,
		client:       client,
		exchangeFunc: exchange,
	}, nil
}

func exchange(m *dns.Msg, client *DNSClient) (r *dns.Msg, rtt time.Duration, err error) {
	for i := 0; i < maxAttempts; i++ {
		if client.d.Net == "https"  {
			return exchangeDOH(m, client.addr)
		}

		r, rtt, err = client.d.Exchange(m, client.addr)
		if err == nil {
			return
		}
	}

	return
}

func exchangeDOH(m *dns.Msg, doh string) (r *dns.Msg, rtt time.Duration, err error) {
	buf, err := m.Pack()
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest(http.MethodPost, doh+"/dns-query", bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("content-type", "application/dns-message")
	req.Header.Set("accept", "application/dns-message")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("error fetching response %s", resp.Status)
	}

	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	ans := new(dns.Msg)
	err = ans.Unpack(b)

	return ans, 0, err
}

func (rs *AD) checkCache(key string, qtype uint16) (*entry, bool) {
	if ans, ok := rs.rrCache[qtype].get(key); ok {
		if time.Now().Before(ans.ttl) {
			return ans, true
		}

		rs.rrCache[qtype].remove(key)
	}

	return nil, false
}

// LookupIP looks up host using the specified resolver.
// It returns a slice of the host's IPv4 and IPv6 addresses.
func (rs *AD) LookupIP(hostname string) ([]net.IP, error) {
	ip := net.ParseIP(hostname)
	if ip != nil {
		return []net.IP{ip}, nil
	}

	if !shouldResolve(hostname) {
		ips, err := net.LookupIP(hostname)
		if err != nil {
			err = fmt.Errorf("ad: ip lookup failed: %v", err)
		}

		return ips, err
	}

	done := make(chan struct{})
	var ipv4, ipv6 []net.IP
	var errIPv4, errIPv6 error

	go func() {
		ipv4, errIPv4 = rs.lookupIPv4(hostname)
		done <- struct{}{}
	}()

	ipv6, errIPv6 = rs.lookupIPv6(hostname)
	<-done

	// return an error only if both lookups fail because some nameservers return servfail for ipv6
	if errIPv4 != nil && errIPv6 != nil {
		return nil, fmt.Errorf("ad: ip lookup failed: [ipv4: %v, ipv6: %v]", errIPv4, errIPv6)
	}

	return append(ipv4, ipv6...), nil
}

func (rs *AD) lookupIPv4(hostname string) ([]net.IP, error) {
	rr, _, err := rs.lookup(hostname, dns.TypeA)
	if err != nil {
		return nil, err
	}

	var ips []net.IP
	for _, r := range rr {
		switch t := r.(type) {
		case *dns.A:
			ips = append(ips, t.A)
		}
	}
	return ips, nil
}

func (rs *AD) lookupIPv6(hostname string) ([]net.IP, error) {
	rr, _, err := rs.lookup(hostname, dns.TypeAAAA)
	if err != nil {
		return nil, err
	}

	var ips []net.IP
	for _, r := range rr {
		switch t := r.(type) {
		case *dns.AAAA:
			ips = append(ips, t.AAAA)
		}
	}
	return ips, nil
}

// LookupTLSA finds the TLSA resource record
func (rs *AD) LookupTLSA(service, proto, name string) ([]*dns.TLSA, error) {
	if net.ParseIP(name) != nil || !shouldResolve(name) {
		return []*dns.TLSA{}, nil
	}

	q, err := dns.TLSAName(dns.Fqdn(name), service, proto)
	if err != nil {
		return nil, err
	}

	rr, ad, err := rs.lookup(q, dns.TypeTLSA)
	if err != nil {
		return nil, fmt.Errorf("ad: tlsa lookup failed: %v", err)
	}

	if !ad {
		return []*dns.TLSA{}, nil
	}

	var tr []*dns.TLSA
	for _, r := range rr {
		switch t := r.(type) {
		case *dns.TLSA:
			tr = append(tr, t)
		}
	}

	return tr, nil
}

func (rs *AD) lookup(name string, qtype uint16) ([]dns.RR, bool, error) {
	if ans, ok := rs.checkCache(name, qtype); ok {
		return ans.msg, ans.secure, nil
	}

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.SetEdns0(4096, false)
	m.RecursionDesired = true
	m.AuthenticatedData = true

	r, _, err := rs.exchangeFunc(m, rs.client)
	if err != nil {
		return nil, false, err
	}

	if rs.Verify != nil {
		if err := rs.Verify(r); err != nil {
			return nil, false, fmt.Errorf("verify error: %v", err)
		}
	}

	if r.Truncated {
		return nil, false, errors.New("response truncated")
	}

	if r.Rcode == dns.RcodeServerFailure {
		return nil, false, errServFail
	}

	if r.Rcode == dns.RcodeSuccess || r.Rcode == dns.RcodeNameError {
		e := &entry{
			msg:    r.Answer,
			secure: r.AuthenticatedData,
			ttl:    time.Now().Add(getMinTTL(r)),
		}

		rs.rrCache[qtype].set(name, e)

		return e.msg, e.secure, nil
	}

	return nil, false, fmt.Errorf("failed with rcode %d", r.Rcode)
}

// getMinTTL get the ttl for dns msg
// borrowed from coredns: https://github.com/coredns/coredns/blob/master/plugin/pkg/dnsutil/ttl.go
func getMinTTL(m *dns.Msg) time.Duration {
	// No records or OPT is the only record, return a short ttl as a fail safe.
	if len(m.Answer)+len(m.Ns) == 0 &&
		(len(m.Extra) == 0 || (len(m.Extra) == 1 && m.Extra[0].Header().Rrtype == dns.TypeOPT)) {
		return minTTL
	}

	minTTL := maxTTL
	for _, r := range m.Answer {
		if r.Header().Ttl < uint32(minTTL.Seconds()) {
			minTTL = time.Duration(r.Header().Ttl) * time.Second
		}
	}
	for _, r := range m.Ns {
		if r.Header().Ttl < uint32(minTTL.Seconds()) {
			minTTL = time.Duration(r.Header().Ttl) * time.Second
		}
	}

	for _, r := range m.Extra {
		if r.Header().Rrtype == dns.TypeOPT {
			// OPT records use TTL field for extended rcode and flags
			continue
		}
		if r.Header().Ttl < uint32(minTTL.Seconds()) {
			minTTL = time.Duration(r.Header().Ttl) * time.Second
		}
	}
	return minTTL
}

func shouldResolve(hostname string) bool {
	var tld string

	index := strings.LastIndex(hostname, ".")
	if index == -1 {
		tld = hostname
	} else {
		tld = hostname[index+1:]
	}

	return tld != "test" && tld != "example" && tld != "invalid" && tld != "localhost"
}
