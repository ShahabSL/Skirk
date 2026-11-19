#ifndef __HEV_UDP_PAYLOAD_H__
#define __HEV_UDP_PAYLOAD_H__

#include <lwip/pbuf.h>

#include "hev-config-const.h"

/* A reassembled UDP datagram can span several pbufs. Keep the ordinary
 * unfragmented path allocation-free and preserve every byte of a chain. */
static inline void *
hev_udp_payload (struct pbuf *p, unsigned char copy[UDP_BUF_SIZE])
{
    if (p->tot_len > UDP_BUF_SIZE)
        return NULL;

    if (p->len == p->tot_len)
        return p->payload;

    if (pbuf_copy_partial (p, copy, p->tot_len, 0) != p->tot_len)
        return NULL;

    return copy;
}

#endif /* __HEV_UDP_PAYLOAD_H__ */
