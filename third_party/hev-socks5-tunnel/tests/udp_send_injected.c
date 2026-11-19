/* Keep production framing; replace only its socket I/O in this test. */
#define hev_task_io_socket_sendmsg test_socket_sendmsg
#define hev_task_io_socket_recv test_socket_recv
#define hev_task_io_socket_recvmsg test_socket_recvmsg
#define hev_task_io_socket_recvfrom test_socket_recvfrom
#include "../src/core/src/hev-socks5-udp.c"
