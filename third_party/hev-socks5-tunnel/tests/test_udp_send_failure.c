#include <assert.h>
#include <stdio.h>

/* Exercise the real static forwarder without a production test hook. */
#define hev_socks5_task_io_yielder test_idle_yielder
#include "../src/hev-socks5-session-udp.c"
#undef hev_socks5_task_io_yielder

enum WriteResult {
    WRITE_FULL, WRITE_SHORT, WRITE_ERROR, WRITE_TIMEOUT, WRITE_PARTIAL_TIMEOUT
};
enum ReadResult {
    READ_HEADER_TIMEOUT, READ_HEADER_PARTIAL_TIMEOUT,
    READ_BODY_TIMEOUT, READ_BODY_PARTIAL_TIMEOUT, READ_UDP_FULL, READ_FULL
};
static enum WriteResult write_result;
static enum ReadResult read_result;
static unsigned char written[128];
static size_t written_len;
static int write_calls, header_reads, body_reads, datagram_reads, idle_waits;
static int pair[2] = {-1, -1};
static size_t header_progress;

int
test_idle_yielder (HevTaskYieldType type, void *data)
{
    (void)data;
    assert (type == HEV_TASK_WAITIO);
    idle_waits++;
    return -1;
}

ssize_t
test_socket_sendmsg (int fd, const struct msghdr *msg, int flags,
                     HevTaskIOYielder yielder, void *data)
{
    size_t length = 0, remaining;

    (void)fd;
    (void)yielder;
    (void)data;
    assert (flags == MSG_WAITALL && msg->msg_iovlen == 3);
    write_calls++;
    for (size_t i = 0; i < msg->msg_iovlen; i++)
        length += msg->msg_iov[i].iov_len;
    if (write_result == WRITE_ERROR) {
        errno = EIO;
        return -1;
    }
    if (write_result == WRITE_TIMEOUT)
        return -2;
    if (write_result == WRITE_SHORT || write_result == WRITE_PARTIAL_TIMEOUT) {
        assert (length > 0);
        length--;
    }
    assert (written_len + length <= sizeof written);
    remaining = length;
    for (size_t i = 0; i < msg->msg_iovlen && remaining; i++) {
        size_t part = msg->msg_iov[i].iov_len;
        if (part > remaining)
            part = remaining;
        memcpy (written + written_len, msg->msg_iov[i].iov_base, part);
        written_len += part;
        remaining -= part;
    }
    return write_result == WRITE_PARTIAL_TIMEOUT ? -2 : (ssize_t)length;
}

ssize_t
test_socket_recv (int fd, void *buf, size_t len, int flags,
                  HevTaskIOYielder yielder, void *data)
{
    static const unsigned char header[] = {0, 5, 10, 1, 127};
    (void)fd;
    (void)yielder;
    (void)data;
    assert ((flags == 0 || flags == MSG_WAITALL) &&
            len == sizeof header - header_progress);
    header_reads++;
    if (read_result == READ_HEADER_TIMEOUT)
        return -2;
    if (read_result == READ_HEADER_PARTIAL_TIMEOUT) {
        if (header_progress)
            return -2;
        memcpy (buf, header, 3);
        header_progress = 3;
        return flags == MSG_WAITALL ? -2 : 3;
    }
    /* Exercise a successful header arriving in several ordinary recv calls. */
    size_t part = flags == MSG_WAITALL ? len : (len > 2 ? 2 : len);
    memcpy (buf, header + header_progress, part);
    header_progress += part;
    return part;
}

ssize_t
test_socket_recvmsg (int fd, struct msghdr *msg, int flags,
                     HevTaskIOYielder yielder, void *data)
{
    static const unsigned char address_tail[] = {0, 0, 1, 0x9c, 0x40};
    (void)fd;
    (void)yielder;
    (void)data;
    body_reads++;
    assert (flags == MSG_WAITALL && msg->msg_iovlen == 2);
    assert (msg->msg_iov[0].iov_len == 5 && msg->msg_iov[1].iov_len == 5);
    assert (read_result == READ_BODY_TIMEOUT || read_result == READ_BODY_PARTIAL_TIMEOUT || read_result == READ_FULL);
    if (read_result != READ_BODY_TIMEOUT) {
        memcpy (msg->msg_iov[0].iov_base, address_tail, sizeof address_tail);
        memcpy (msg->msg_iov[1].iov_base, "first", read_result == READ_FULL ? 5 : 3);
    }
    if (read_result == READ_FULL) {
        header_progress = 0;
        return 10;
    }
    return -2;
}

ssize_t
test_socket_recvfrom (int fd, void *buf, size_t len, int flags,
                      struct sockaddr *addr, socklen_t *addrlen,
                      HevTaskIOYielder yielder, void *data)
{
    static const unsigned char record[] = {
        0, 0, 0, 1, 127, 0, 0, 1, 0x9c, 0x40, 'f', 'i', 'r', 's', 't'
    };
    (void)fd;
    (void)addrlen;
    (void)yielder;
    (void)data;
    assert (!flags && !addr && len >= sizeof record);
    datagram_reads++;
    if (read_result != READ_UDP_FULL)
        return -2;
    memcpy (buf, record, sizeof record);
    return sizeof record;
}

static void
prepare (HevSocks5SessionUDP *self, enum WriteResult result)
{
    if (pair[0] >= 0) {
        close (pair[0]);
        close (pair[1]);
    }
    assert (socketpair (AF_UNIX, SOCK_STREAM, 0, pair) == 0);
    memset (self, 0, sizeof *self);
    HEV_OBJECT (self)->klass = HEV_SOCKS5_SESSION_UDP_TYPE;
    HEV_SOCKS5 (self)->type = HEV_SOCKS5_TYPE_UDP_IN_TCP;
    HEV_SOCKS5 (self)->fd = pair[0];
    HEV_SOCKS5_CLIENT_UDP (self)->fd = pair[0];
    hev_socks5_set_timeout (HEV_SOCKS5 (self), 1000);
    self->alive = HEV_SOCKS5_SESSION_UDP_ALIVE_F |
                  HEV_SOCKS5_SESSION_UDP_ALIVE_B;
    write_result = result;
    write_calls = 0;
    written_len = 0;
    header_reads = body_reads = datagram_reads = idle_waits = 0;
    header_progress = 0;
}

static void
assert_stream_shutdown (void)
{
    unsigned char byte;
    assert (recv (pair[1], &byte, 1, MSG_DONTWAIT) == 0);
    assert (recv (pair[0], &byte, 1, MSG_DONTWAIT) == 0);
}

static void
assert_stream_open (void)
{
    unsigned char byte;
    assert (recv (pair[1], &byte, 1, MSG_DONTWAIT) == -1 && errno == EAGAIN);
}

static HevSocks5UDPFrame *
queue_frame (HevSocks5SessionUDP *self, const char *body)
{
    HevSocks5UDPFrame *frame = hev_malloc0 (sizeof *frame);
    size_t length = strlen (body);

    assert (frame);
    frame->addr.atype = HEV_SOCKS5_ADDR_TYPE_IPV4;
    frame->addr.ipv4.addr[0] = 127;
    frame->addr.ipv4.addr[3] = 1;
    frame->addr.ipv4.port = htons (40000);
    frame->data = pbuf_alloc (PBUF_RAW, length, PBUF_RAM);
    assert (frame->data);
    assert (pbuf_take (frame->data, body, length) == ERR_OK);
    hev_list_add_tail (&self->frame_list, &frame->node);
    self->frames++;
    return frame;
}

static void
discard_queued (HevSocks5SessionUDP *self)
{
    HevListNode *node;
    while ((node = hev_list_first (&self->frame_list))) {
        HevSocks5UDPFrame *frame = container_of (node, HevSocks5UDPFrame, node);
        hev_list_del (&self->frame_list, node);
        pbuf_free (frame->data);
        hev_free (frame);
        self->frames--;
    }
    assert (self->frames == 0);
}

static void
fatal_write (enum WriteResult result)
{
    static const unsigned char first[] = {
        0, 5, 10, 1, 127, 0, 0, 1, 0x9c, 0x40, 'f', 'i', 'r', 's', 't'
    };
    HevSocks5SessionUDP self;
    HevSocks5UDPFrame *second;
    int stopped = 0;

    prepare (&self, result);
    queue_frame (&self, "first");
    second = queue_frame (&self, "second");
    /* The production splice loop stops on a negative forwarder result. */
    for (int i = 0; i < 2; i++) {
        if (hev_socks5_session_udp_fwd_f (&self) < 0) {
            stopped = 1;
            break;
        }
    }
    assert (stopped && write_calls == 1 && self.frames == 1);
    assert (hev_list_first (&self.frame_list) == &second->node);
    assert (hev_socks5_get_timeout (HEV_SOCKS5 (&self)) == 0);
    assert_stream_shutdown ();
    if (result == WRITE_SHORT || result == WRITE_PARTIAL_TIMEOUT) {
        assert (written_len == sizeof first - 1);
        assert (!memcmp (written, first, written_len));
    } else {
        assert (written_len == 0);
    }
    discard_queued (&self);
}

static void
successful_empty_then_nonempty (void)
{
    static const unsigned char expected[] = {
        0, 0, 10, 1, 127, 0, 0, 1, 0x9c, 0x40,
        0, 6, 10, 1, 127, 0, 0, 1, 0x9c, 0x40, 's', 'e', 'c', 'o', 'n', 'd'
    };
    HevSocks5SessionUDP self;

    prepare (&self, WRITE_FULL);
    queue_frame (&self, "");
    queue_frame (&self, "second");
    assert (hev_socks5_session_udp_fwd_f (&self) == 0);
    assert (hev_socks5_session_udp_fwd_f (&self) == 0);
    assert (self.frames == 0 && !hev_list_first (&self.frame_list));
    assert (write_calls == 2 && written_len == sizeof expected);
    assert (!memcmp (written, expected, sizeof expected));
    assert (hev_socks5_get_timeout (HEV_SOCKS5 (&self)) == 1000);
    assert_stream_open ();
}

static void
datagram_timeout_preserves_alive_policy (void)
{
    HevSocks5SessionUDP self;

    prepare (&self, WRITE_TIMEOUT);
    HEV_SOCKS5 (&self)->type = HEV_SOCKS5_TYPE_UDP_IN_UDP;
    queue_frame (&self, "first");
    queue_frame (&self, "second");
    assert (hev_socks5_session_udp_fwd_f (&self) == 0);
    assert (self.frames == 1 && write_calls == 1 && written_len == 0);
    assert (self.alive == HEV_SOCKS5_SESSION_UDP_ALIVE_B);
    assert (hev_socks5_get_timeout (HEV_SOCKS5 (&self)) == 1000);
    write_result = WRITE_FULL;
    assert (hev_socks5_session_udp_fwd_f (&self) == 0);
    assert (self.frames == 0 && write_calls == 2);
    assert (self.alive == (HEV_SOCKS5_SESSION_UDP_ALIVE_F |
                          HEV_SOCKS5_SESSION_UDP_ALIVE_B));
    assert_stream_open ();

    prepare (&self, WRITE_TIMEOUT);
    HEV_SOCKS5 (&self)->type = HEV_SOCKS5_TYPE_UDP_IN_UDP;
    self.alive = HEV_SOCKS5_SESSION_UDP_ALIVE_F;
    queue_frame (&self, "first");
    queue_frame (&self, "second");
    assert (hev_socks5_session_udp_fwd_f (&self) < 0);
    assert (self.alive == 0 && self.frames == 1 && write_calls == 1);
    assert (hev_socks5_get_timeout (HEV_SOCKS5 (&self)) == 1000);
    assert_stream_open ();
    discard_queued (&self);
}

static void
fatal_read_timeout (enum ReadResult result)
{
    HevSocks5SessionUDP self;
    prepare (&self, WRITE_FULL);
    read_result = result;
    /* A negative result stops the production reader loop before the next header. */
    assert (hev_socks5_session_udp_fwd_b (&self) < 0);
    assert (header_reads == (result == READ_HEADER_PARTIAL_TIMEOUT ? 2 : 3));
    assert (body_reads == (result == READ_BODY_TIMEOUT || result == READ_BODY_PARTIAL_TIMEOUT));
    assert (hev_socks5_get_timeout (HEV_SOCKS5 (&self)) == 0);
    assert_stream_shutdown ();
}

static void
untouched_header_timeout_can_continue (void)
{
    HevSocks5SessionUDP self;
    HevSocks5Addr addr;
    unsigned char body[16];

    prepare (&self, WRITE_FULL);
    read_result = READ_HEADER_TIMEOUT;
    assert (hev_socks5_session_udp_fwd_b (&self) == 0);
    assert (header_reads == 1 && body_reads == 0);
    assert (self.alive == HEV_SOCKS5_SESSION_UDP_ALIVE_F);
    assert (hev_socks5_get_timeout (HEV_SOCKS5 (&self)) == 1000);
    assert_stream_open ();
    read_result = READ_FULL;
    assert (hev_socks5_udp_recvfrom (HEV_SOCKS5_UDP (&self), body, sizeof body, &addr) == 5);
    assert (!memcmp (body, "first", 5) && header_reads == 4 && body_reads == 1);
    assert (addr.atype == HEV_SOCKS5_ADDR_TYPE_IPV4 && ntohs (addr.ipv4.port) == 40000);
    assert_stream_open ();
}

static void
datagram_read_timeout_can_continue (void)
{
    HevSocks5SessionUDP self;
    HevSocks5Addr addr;
    unsigned char body[16];

    prepare (&self, WRITE_FULL);
    HEV_SOCKS5 (&self)->type = HEV_SOCKS5_TYPE_UDP_IN_UDP;
    HEV_SOCKS5 (&self)->udp_associated = 1;
    read_result = READ_BODY_TIMEOUT;
    assert (hev_socks5_session_udp_fwd_b (&self) == 0);
    assert (self.alive == HEV_SOCKS5_SESSION_UDP_ALIVE_F);
    assert (hev_socks5_get_timeout (HEV_SOCKS5 (&self)) == 1000);
    read_result = READ_UDP_FULL;
    assert (hev_socks5_udp_recvfrom (HEV_SOCKS5_UDP (&self), body, sizeof body, &addr) == 5);
    assert (!memcmp (body, "first", 5) && datagram_reads == 2);
    assert (addr.atype == HEV_SOCKS5_ADDR_TYPE_IPV4 && ntohs (addr.ipv4.port) == 40000);
    assert_stream_open ();
}

static void
idle_queue_wait_can_continue (void)
{
    HevSocks5SessionUDP self;

    prepare (&self, WRITE_FULL);
    assert (hev_socks5_session_udp_fwd_f (&self) == 0);
    assert (idle_waits == 1 && write_calls == 0 && self.frames == 0);
    assert (self.alive == HEV_SOCKS5_SESSION_UDP_ALIVE_B);
    assert (hev_socks5_get_timeout (HEV_SOCKS5 (&self)) == 1000);
    queue_frame (&self, "first");
    assert (hev_socks5_session_udp_fwd_f (&self) == 0);
    assert (write_calls == 1 && self.frames == 0);
    assert (self.alive == (HEV_SOCKS5_SESSION_UDP_ALIVE_F |
                          HEV_SOCKS5_SESSION_UDP_ALIVE_B));
    assert_stream_open ();
}

int
main (int argc, char **argv)
{
    /* Allow isolated negative controls to prove each framing regression. */
    if (argc == 2) {
        if (!strcmp (argv[1], "write-timeout"))
            fatal_write (WRITE_PARTIAL_TIMEOUT);
        else if (!strcmp (argv[1], "read-timeout"))
            fatal_read_timeout (READ_BODY_PARTIAL_TIMEOUT);
        else
            return 2;
        return 0;
    }
    fatal_write (WRITE_SHORT);
    fatal_write (WRITE_ERROR);
    fatal_write (WRITE_TIMEOUT);
    fatal_write (WRITE_PARTIAL_TIMEOUT);
    fatal_read_timeout (READ_HEADER_PARTIAL_TIMEOUT);
    fatal_read_timeout (READ_BODY_TIMEOUT);
    fatal_read_timeout (READ_BODY_PARTIAL_TIMEOUT);
    untouched_header_timeout_can_continue ();
    successful_empty_then_nonempty ();
    datagram_timeout_preserves_alive_policy ();
    datagram_read_timeout_can_continue ();
    idle_queue_wait_can_continue ();
    close (pair[0]);
    close (pair[1]);
    puts ("Native HEV partial framed I/O shuts down both directions; empty frames and untouched timeouts passed");
    return 0;
}
