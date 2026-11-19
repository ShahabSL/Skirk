/* Run the native tunnel with an already open test socket instead of a TUN. */
#include <limits.h>
#include <stdlib.h>

#include "hev-socks5-tunnel.h"

int
main (int argc, char **argv)
{
    char *end;
    long fd;

    if (argc != 3)
        return 2;

    fd = strtol (argv[2], &end, 10);
    if (*end || end == argv[2] || fd < 0 || fd > INT_MAX)
        return 2;

    return hev_socks5_tunnel_main_from_file (argv[1], (int)fd) == 0 ? 0 : 1;
}
