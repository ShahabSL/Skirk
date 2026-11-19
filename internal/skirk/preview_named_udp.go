package skirk

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	previewNamedMaxAnswers = 64
	previewNamedMaxAliases = 8
	previewNamedRetry      = 2 * time.Second
)

// Association readers own this bounded table. Successful admissions pin a
// destination for the association; DNS TTL expiry must not migrate live UDP.
type previewNamedAdmission struct {
	target  netip.AddrPort
	retryAt time.Time
}

func (r *pilotClientRelay) allowedTarget(ctx context.Context, packet socksUDPPacket) (netip.AddrPort, bool) {
	if _, err := netip.ParseAddr(packet.Host); err == nil {
		return pilotAllowedTarget(r.allowed, r.publicIPv4, r.publicIPv6, packet)
	}
	if !r.namedUDP || r.remoteDNS == nil || packet.Port < 1 || packet.Port > 65535 ||
		!previewUDPAllowedPort(uint16(packet.Port)) || len(packet.Payload) > pilotMaxPayload || ctx.Err() != nil {
		return netip.AddrPort{}, false
	}
	name, ok := previewNamedCanonical(packet.Host)
	if !ok {
		return netip.AddrPort{}, false
	}
	key := name + ":" + strconv.Itoa(packet.Port)
	now := time.Now()
	if prior, found := r.namedTargets[key]; found {
		if prior.target.IsValid() {
			return prior.target, true
		}
		if now.Before(prior.retryAt) {
			return netip.AddrPort{}, false
		}
	} else if len(r.namedTargets) >= pilotMaxFlows {
		// A resolver outage must not permanently consume the association's
		// destination budget. Successful addresses stay pinned for live flows.
		for name, entry := range r.namedTargets {
			if !entry.target.IsValid() && !now.Before(entry.retryAt) {
				delete(r.namedTargets, name)
			}
		}
		if len(r.namedTargets) >= pilotMaxFlows {
			return netip.AddrPort{}, false
		}
	}
	if r.namedTargets == nil {
		r.namedTargets = make(map[string]previewNamedAdmission)
	}
	r.namedTargets[key] = previewNamedAdmission{retryAt: time.Now().Add(previewNamedRetry)}
	ctx, cancel := context.WithTimeout(ctx, previewDNSTimeout)
	defer cancel()
	for _, family := range []struct {
		enabled bool
		kind    dnsmessage.Type
	}{
		{r.publicIPv4, dnsmessage.TypeA}, {r.publicIPv6, dnsmessage.TypeAAAA},
	} {
		if !family.enabled || ctx.Err() != nil {
			continue
		}
		query, err := previewNamedQuery(name, family.kind)
		if err != nil {
			continue
		}
		wire, err := query.Pack()
		if err != nil {
			continue
		}
		response, err := r.remoteDNS(ctx, wire, false)
		if err != nil || ctx.Err() != nil {
			continue
		}
		for _, address := range previewNamedAddresses(query, response) {
			target := netip.AddrPortFrom(address, uint16(packet.Port))
			// DNS names never inherit the loopback test-fixture exceptions.
			if permitPreviewTarget(nil, r.publicIPv4, r.publicIPv6, target) {
				r.namedTargets[key] = previewNamedAdmission{target: target}
				return target, true
			}
		}
	}
	r.namedTargets[key] = previewNamedAdmission{retryAt: time.Now().Add(previewNamedRetry)}
	return netip.AddrPort{}, false
}

func previewNamedCanonical(host string) (string, bool) {
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return "", false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 {
			return "", false
		}
		for _, c := range []byte(label) {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", false
			}
		}
	}
	return strings.ToLower(host) + ".", true
}

func previewNamedQuery(host string, kind dnsmessage.Type) (dnsmessage.Message, error) {
	name, err := dnsmessage.NewName(host)
	if err != nil {
		return dnsmessage.Message{}, err
	}
	var id [2]byte
	if _, err := rand.Read(id[:]); err != nil {
		return dnsmessage.Message{}, err
	}
	return dnsmessage.Message{
		Header:    dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: kind, Class: dnsmessage.ClassINET}},
		Additionals: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: dnsmessage.Class(previewDNSUDPMax)},
			Body:   &dnsmessage.OPTResource{},
		}},
	}, nil
}

func previewNamedAddresses(query dnsmessage.Message, wire []byte) []netip.Addr {
	if len(wire) > 65535 || len(query.Questions) != 1 {
		return nil
	}
	var reply dnsmessage.Message
	if reply.Unpack(wire) != nil || !reply.Response || reply.ID != query.ID || reply.OpCode != 0 ||
		reply.Truncated || reply.RCode != dnsmessage.RCodeSuccess || len(reply.Questions) != 1 ||
		len(reply.Answers) > previewNamedMaxAnswers {
		return nil
	}
	question := query.Questions[0]
	if reply.Questions[0].Type != question.Type || reply.Questions[0].Class != question.Class ||
		!strings.EqualFold(reply.Questions[0].Name.String(), question.Name.String()) {
		return nil
	}
	current := strings.ToLower(question.Name.String())
	seen := make(map[string]bool)
	for depth := 0; depth <= previewNamedMaxAliases; depth++ {
		if seen[current] {
			return nil
		}
		seen[current] = true
		var alias string
		var addresses []netip.Addr
		for _, answer := range reply.Answers {
			if answer.Header.Class != dnsmessage.ClassINET || !strings.EqualFold(answer.Header.Name.String(), current) {
				continue
			}
			switch body := answer.Body.(type) {
			case *dnsmessage.CNAMEResource:
				name, valid := previewNamedCanonical(body.CNAME.String())
				if !valid || alias != "" && alias != name {
					return nil
				}
				alias = name
			case *dnsmessage.AResource:
				if question.Type == dnsmessage.TypeA {
					addresses = append(addresses, netip.AddrFrom4(body.A))
				}
			case *dnsmessage.AAAAResource:
				if question.Type == dnsmessage.TypeAAAA {
					addresses = append(addresses, netip.AddrFrom16(body.AAAA))
				}
			}
		}
		if alias == "" {
			return addresses
		}
		if len(addresses) != 0 {
			return nil
		}
		current = alias
	}
	return nil
}
