package skirk

import (
	"bytes"
	"context"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Owned by one running client and discarded on disconnect or profile switch.
// Each stored response is at most previewDNSUDPMax bytes.
const previewDNSCacheEntries = 256

// RFC 2308 §5 recommends limiting negative answers to one to three hours.
const maxNegativeDNSTTL = 3 * 60 * 60

type previewDNSCache struct {
	mu      sync.Mutex
	answers map[string]previewDNSAnswer
	pending map[string]*previewDNSLookup
}

type previewDNSAnswer struct {
	wire        []byte
	stored      time.Time
	expires     time.Time
	negativeTTL uint32
}

type previewDNSLookup struct {
	done     chan struct{}
	answer   previewDNSAnswer
	err      error
	canceled bool
}

func newPreviewDNSCache() *previewDNSCache {
	return &previewDNSCache{
		answers: make(map[string]previewDNSAnswer),
		pending: make(map[string]*previewDNSLookup),
	}
}

func plainDNSOptions(msg dnsmessage.Message) bool {
	if len(msg.Additionals) == 0 {
		return true
	}
	if len(msg.Additionals) != 1 {
		return false
	}
	rr := msg.Additionals[0]
	opt, ok := rr.Body.(*dnsmessage.OPTResource)
	return ok && rr.Header.Name.String() == "." && rr.Header.TTL == 0 && len(opt.Options) == 0
}

func cacheableAddressQuestion(msg dnsmessage.Message) bool {
	q := msg.Questions[0] // unpackPreviewDNSQuery requires a single standard question.
	return q.Class == dnsmessage.ClassINET && (q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeAAAA) &&
		msg.RecursionDesired && !msg.RecursionAvailable && !msg.Authoritative && msg.RCode == dnsmessage.RCodeSuccess &&
		!msg.AuthenticData && !msg.CheckingDisabled && !msg.Truncated && plainDNSOptions(msg)
}

func newPreviewDNSAnswer(wire []byte, started time.Time) previewDNSAnswer {
	var msg dnsmessage.Message
	if len(wire) > previewDNSUDPMax || msg.Unpack(wire) != nil ||
		!msg.Response || msg.Truncated || msg.AuthenticData || msg.CheckingDisabled ||
		len(msg.Questions) != 1 || !plainDNSOptions(msg) {
		return previewDNSAnswer{}
	}
	ttl := previewDNSAnswerTTL(msg)
	if ttl == 0 {
		return previewDNSAnswer{}
	}
	// Charge the Drive round trip against the TTL as well.
	answer := previewDNSAnswer{wire: bytes.Clone(wire), stored: started, expires: started.Add(time.Duration(ttl) * time.Second)}
	if len(msg.Answers) == 0 {
		answer.negativeTTL = ttl
	}
	return answer
}

func previewDNSAnswerTTL(msg dnsmessage.Message) uint32 {
	if len(msg.Answers) == 0 && len(msg.Authorities) == 1 &&
		(msg.RCode == dnsmessage.RCodeSuccess || msg.RCode == dnsmessage.RCodeNameError) {
		// Only cache a clear denial for this exact question. Leave referrals,
		// signed proofs and negative CNAME chains to the upstream resolver.
		rr := msg.Authorities[0]
		soa, ok := rr.Body.(*dnsmessage.SOAResource)
		if !ok || rr.Header.Class != dnsmessage.ClassINET || rr.Header.TTL > 1<<31-1 || soa.MinTTL > 1<<31-1 ||
			!dnsNameWithinZone(msg.Questions[0].Name, rr.Header.Name) {
			return 0
		}
		return min(rr.Header.TTL, soa.MinTTL, maxNegativeDNSTTL)
	}
	if msg.RCode != dnsmessage.RCodeSuccess || len(msg.Authorities) != 0 {
		return 0
	}
	qtype := msg.Questions[0].Type
	ttl := uint32(1<<31 - 1)
	foundAddress := false
	for _, rr := range msg.Answers {
		if rr.Header.Class != dnsmessage.ClassINET || rr.Header.TTL == 0 || rr.Header.TTL > 1<<31-1 ||
			(rr.Header.Type != qtype && rr.Header.Type != dnsmessage.TypeCNAME) {
			return 0
		}
		foundAddress = foundAddress || rr.Header.Type == qtype
		ttl = min(ttl, rr.Header.TTL)
	}
	if !foundAddress {
		return 0
	}
	return ttl
}

func dnsNameWithinZone(name, zone dnsmessage.Name) bool {
	if zone.Length == 1 && zone.Data[0] == '.' {
		return true
	}
	start := int(name.Length) - int(zone.Length)
	if start < 0 || (start > 0 && name.Data[start-1] != '.') {
		return false
	}
	// DNS folds only ASCII letters; all other label octets compare exactly.
	for i, z := range zone.Data[:zone.Length] {
		n := name.Data[start+i]
		if n == z {
			continue
		}
		if n^z != 0x20 || z|0x20 < 'a' || z|0x20 > 'z' {
			return false
		}
	}
	return true
}

func (answer previewDNSAnswer) reply(id uint16, now time.Time) ([]byte, error) {
	var msg dnsmessage.Message
	if err := msg.Unpack(answer.wire); err != nil {
		return nil, err
	}
	msg.ID = id
	age := uint32(now.Sub(answer.stored) / time.Second)
	for i := range msg.Answers {
		msg.Answers[i].Header.TTL -= age
	}
	if ttl := answer.negativeTTL; ttl != 0 {
		msg.Authorities[0].Header.TTL = ttl - min(ttl, age)
	}
	return msg.Pack()
}

// Called with mu held. A stream of unique misses must not evict live addresses.
func (c *previewDNSCache) store(key string, answer previewDNSAnswer) {
	if len(c.answers) >= previewDNSCacheEntries {
		now := time.Now()
		var evict string
		var earliest time.Time
		for k, v := range c.answers {
			if !now.Before(v.expires) || v.negativeTTL != 0 {
				evict = k
				break
			}
			if answer.negativeTTL == 0 && (earliest.IsZero() || v.expires.Before(earliest)) {
				evict, earliest = k, v.expires
			}
		}
		if evict == "" {
			return
		}
		delete(c.answers, evict)
	}
	c.answers[key] = answer
}

func (d *previewDNS) cachedExchange(ctx context.Context, query []byte, request dnsmessage.Message) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, previewDNSTimeout)
	defer cancel()
	c := d.cache
	key := string(query[2:]) // Keep question case, header flags and EDNS size. Only the ID differs.
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := d.ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		now := time.Now()
		if answer, ok := c.answers[key]; ok {
			if now.Before(answer.expires) {
				c.mu.Unlock()
				return answer.reply(request.ID, now)
			}
			delete(c.answers, key)
		}
		if lookup := c.pending[key]; lookup != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-d.ctx.Done():
				return nil, d.ctx.Err()
			case <-lookup.done:
			}
			if lookup.canceled {
				continue // Another caller closed its connection; this caller can still resolve.
			}
			if lookup.err != nil {
				return nil, lookup.err
			}
			if now := time.Now(); now.Before(lookup.answer.expires) {
				return lookup.answer.reply(request.ID, now)
			}
			// The first response could not be reused.
			return d.exchange(ctx, query, request)
		}
		if len(c.pending) >= previewDNSCacheEntries {
			c.mu.Unlock()
			return d.exchange(ctx, query, request)
		}
		lookup := &previewDNSLookup{done: make(chan struct{})}
		c.pending[key] = lookup
		c.mu.Unlock()

		wire, err := d.exchange(ctx, query, request)
		if err == nil {
			lookup.answer = newPreviewDNSAnswer(wire, now)
			if lookup.answer.negativeTTL != 0 {
				wire, err = lookup.answer.reply(request.ID, time.Now())
			}
		}
		c.mu.Lock()
		if err == nil && time.Now().Before(lookup.answer.expires) {
			c.store(key, lookup.answer)
		}
		lookup.err = err
		lookup.canceled = err != nil && ctx.Err() != nil
		delete(c.pending, key)
		close(lookup.done)
		c.mu.Unlock()
		return wire, err
	}
}
