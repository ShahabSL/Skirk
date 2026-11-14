#include <assert.h>
#include <arpa/inet.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include <lwip/ip_addr.h>
#include <hev-socks5-misc.h>
#include "src/hev-config.h"
#include "src/hev-mapped-dns.h"
#include "src/misc/hev-utils.h"

static int
make_query (uint8_t *out, uint16_t type, int edns)
{
    static const uint8_t qname[] = {
        7, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
        3, 'c', 'o', 'm', 0
    };
    int n = 12;
    memset (out, 0, 512);
    out[0] = 0x12;
    out[1] = 0x34;
    out[2] = 0x01;
    out[5] = 1;
    out[11] = edns;
    memcpy (out + n, qname, sizeof (qname));
    n += sizeof (qname);
    out[n++] = type >> 8;
    out[n++] = type;
    out[n++] = 0;
    out[n++] = 1;
    if (edns) {
        static const uint8_t opt[] = {0, 0, 41, 4, 208, 0, 0, 0, 0, 0, 0};
        memcpy (out + n, opt, sizeof (opt));
        n += sizeof (opt);
    }
    return n;
}

static uint16_t
u16 (const uint8_t *p)
{
    return ((uint16_t)p[0] << 8) | p[1];
}

int
main (void)
{
    static const char ipv6_config[] =
        "mapdns:\n  ipv6-network: 'fd9d:36cb:b8bb:1::'\n";
    static const char ipv4_config[] =
        "mapdns:\n  network: 240.0.0.0\n";
    static const char invalid_config[] =
        "mapdns:\n  ipv6-network: '2001:db8::'\n";
    HevMappedDNS *dns = hev_mapped_dns_new (0xf0000000, 0xf0000000, 100);
    uint8_t query[512], response[512];
    ip_addr_t ip = {0};
    HevSocks5Addr addr = {0};
    const uint8_t network[16] = {
        0xfd, 0x9d, 0x36, 0xcb, 0xb8, 0xbb, 0, 1,
        0, 0, 0, 0, 0, 0, 0, 0
    };
    int qlen, rlen;

    assert (dns);
    assert (hev_config_init_from_str ((const unsigned char *)ipv6_config,
                                      sizeof ipv6_config - 1) == 0);
    assert (hev_config_get_mapdns_ipv6_network ());
    assert (!memcmp (hev_config_get_mapdns_ipv6_network (), network, 16));
    assert (hev_config_init_from_str ((const unsigned char *)ipv4_config,
                                      sizeof ipv4_config - 1) == 0);
    assert (!hev_config_get_mapdns_ipv6_network ());
    assert (hev_config_init_from_str ((const unsigned char *)invalid_config,
                                      sizeof invalid_config - 1) < 0);
    hev_mapped_dns_put (dns);

    qlen = make_query (query, 1, 1);
    rlen = hev_mapped_dns_handle (dns, query, qlen, response, sizeof response);
    assert (rlen == 45); /* 29-byte question + 16-byte A answer */
    assert (u16 (response + 6) == 1);
    assert (u16 (response + 10) == 0); /* EDNS OPT was dropped. */
    assert (u16 (response + 29 + 2) == 1);
    assert (u16 (response + 29 + 10) == 4);
    assert (!memcmp (response + 29 + 12, "\xf0\0\0\0", 4));

    qlen = make_query (query, 28, 0);
    rlen = hev_mapped_dns_handle (dns, query, qlen, response, sizeof response);
    assert (rlen == 29 && u16 (response + 6) == 0); /* IPv4-only default. */

    hev_mapped_dns_set_ipv6_network (dns, network);
    qlen = make_query (query, 28, 0);
    rlen = hev_mapped_dns_handle (dns, query, qlen, response, sizeof response);
    assert (rlen == 57); /* 29-byte question + 28-byte AAAA answer */
    assert (u16 (response + 6) == 1);
    assert (u16 (response + 29 + 2) == 28);
    assert (u16 (response + 29 + 10) == 16);
    assert (!memcmp (response + 29 + 12, network, 12));
    assert (!memcmp (response + 29 + 24, "\xf0\0\0\0", 4));

    qlen = make_query (query, 65, 0); /* Unsupported HTTPS RR first. */
    memcpy (query + qlen, query + 12, 17);
    query[qlen + 13] = 0;
    query[qlen + 14] = 28;
    qlen += 17;
    query[5] = 2;
    rlen = hev_mapped_dns_handle (dns, query, qlen, response, sizeof response);
    assert (rlen == 74 && u16 (response + 6) == 1);
    assert (response[46] == 0xc0 && response[47] == 29);
    assert (u16 (response + 48) == 28);

    qlen = make_query (query, 28, 0);
    rlen = hev_mapped_dns_handle (dns, query, qlen, response, sizeof response);
    assert (rlen == 57);

    memcpy (ip_2_ip6 (&ip)->addr, response + 29 + 12, 16);
    ip.type = IPADDR_TYPE_V6;
    assert (hev_socks5_addr_from_lwip (&addr, &ip, 443) == 0);
    assert (addr.atype == HEV_SOCKS5_ADDR_TYPE_NAME);
    assert (addr.domain.len == 11);
    assert (!memcmp (addr.domain.addr, "example.com", 11));
    assert (addr.domain.addr[11] == 1 && addr.domain.addr[12] == 0xbb);

    ip_2_ip6 (&ip)->addr[0] = htonl (0x20010db8);
    assert (hev_socks5_addr_from_lwip (&addr, &ip, 443) == 0);
    assert (addr.atype == HEV_SOCKS5_ADDR_TYPE_IPV6);

    ip.type = IPADDR_TYPE_V4;
    ip_2_ip4 (&ip)->addr = htonl (0xf0000000);
    assert (hev_socks5_addr_from_lwip (&addr, &ip, 443) == 0);
    assert (addr.atype == HEV_SOCKS5_ADDR_TYPE_NAME);
    ip_2_ip4 (&ip)->addr = htonl (0x08080808);
    assert (hev_socks5_addr_from_lwip (&addr, &ip, 443) == 0);
    assert (addr.atype == HEV_SOCKS5_ADDR_TYPE_IPV4);

    assert (hev_mapped_dns_handle (dns, query, 0, response, sizeof response) < 0);
    memset (query, 0, sizeof query);
    query[5] = 1;
    qlen = 12;
    for (int part = 0; part < 4; part++) {
        int label_size = part == 3 ? 61 : 63;
        query[qlen++] = label_size;
        memset (query + qlen, 'a' + part, label_size);
        qlen += label_size;
    }
    query[qlen++] = 0;
    query[qlen++] = 0;
    query[qlen++] = 1;
    query[qlen++] = 0;
    query[qlen++] = 1;
    assert (hev_mapped_dns_handle (dns, query, qlen, response,
                                   sizeof response) > 0);
    memset (query, 0, sizeof query);
    query[5] = 1;
    qlen = 12;
    for (int part = 0; part < 4; part++) {
        query[qlen++] = 63;
        memset (query + qlen, 'a' + part, 63);
        qlen += 63;
    }
    query[qlen++] = 0;
    query[qlen++] = 0;
    query[qlen++] = 1;
    query[qlen++] = 0;
    query[qlen++] = 1;
    /* This 257-byte QNAME exceeds DNS's limit. */
    assert (hev_mapped_dns_handle (dns, query, qlen, response,
                                   sizeof response) < 0);
    query[12] = 64; /* A label may not exceed 63 bytes. */
    assert (hev_mapped_dns_handle (dns, query, qlen, response,
                                   sizeof response) < 0);
    {
        uint32_t seed = 0x534b4952;
        for (int trial = 0; trial < 50000; trial++) {
            int length;
            for (int j = 0; j < (int)sizeof query; j++) {
                seed = seed * 1664525u + 1013904223u;
                query[j] = seed >> 24;
            }
            length = seed % sizeof query;
            if (length >= 6) {
                query[4] = 0;
                query[5] = 1;
            }
            rlen = hev_mapped_dns_handle (dns, query, length, response,
                                          sizeof response);
            assert (rlen < 0 || rlen <= (int)sizeof response);
        }
    }
    puts ("PASS config opt-in/reset/validation, A, opt-in AAAA, mixed questions, EDNS count, SOCKS mapping, IPv4/IPv6 literals, DNS name bounds, 50k malformed packets");
    return 0;
}
