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

type previewDNSCache struct {
	mu      sync.Mutex
	answers map[string]previewDNSAnswer
	pending map[string]*previewDNSLookup
}

type previewDNSAnswer struct {
	wire    []byte
	stored  time.Time
	expires time.Time
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
		msg.RCode != dnsmessage.RCodeSuccess || msg.Truncated || msg.AuthenticData ||
		len(msg.Questions) != 1 || len(msg.Answers) == 0 || len(msg.Authorities) != 0 || !plainDNSOptions(msg) {
		return previewDNSAnswer{}
	}
	qtype := msg.Questions[0].Type
	ttl := uint32(1<<31 - 1)
	foundAddress := false
	for _, rr := range msg.Answers {
		if rr.Header.Class != dnsmessage.ClassINET || rr.Header.TTL == 0 || rr.Header.TTL > 1<<31-1 ||
			(rr.Header.Type != qtype && rr.Header.Type != dnsmessage.TypeCNAME) {
			return previewDNSAnswer{}
		}
		foundAddress = foundAddress || rr.Header.Type == qtype
		ttl = min(ttl, rr.Header.TTL)
	}
	if !foundAddress {
		return previewDNSAnswer{}
	}
	// Charge the Drive round trip against the TTL as well.
	return previewDNSAnswer{wire: bytes.Clone(wire), stored: started, expires: started.Add(time.Duration(ttl) * time.Second)}
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
	return msg.Pack()
}

// Called with mu held. Prefer evicting the answer that expires soonest.
func (c *previewDNSCache) store(key string, answer previewDNSAnswer) {
	if len(c.answers) >= previewDNSCacheEntries {
		var evict string
		var earliest time.Time
		for k, v := range c.answers {
			if earliest.IsZero() || v.expires.Before(earliest) {
				evict, earliest = k, v.expires
			}
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
			// The first response had no reusable positive answer.
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
		c.mu.Lock()
		if err == nil {
			lookup.answer = newPreviewDNSAnswer(wire, now)
			if time.Now().Before(lookup.answer.expires) {
				c.store(key, lookup.answer)
			}
		}
		lookup.err = err
		lookup.canceled = err != nil && ctx.Err() != nil
		delete(c.pending, key)
		close(lookup.done)
		c.mu.Unlock()
		return wire, err
	}
}
