#ifndef __TRACER_TELEMETRY_H
#define __TRACER_TELEMETRY_H

#include "tracer-maps.h"
#include "bpf_endian.h"
#include "kconfig.h"
#include <net/sock.h>

enum telemetry_counter
{
    missed_tcp_close,
    missed_udp_close,
    udp_send_processed,
    udp_send_missed,
};

static __always_inline void increment_telemetry_count(enum telemetry_counter counter_name) {
    __u64 key = 0;
    telemetry_t *val = NULL;
    val = bpf_map_lookup_elem(&telemetry, &key);
    if (val == NULL) {
        return;
    }

    switch (counter_name) {
    case missed_tcp_close:
        __sync_fetch_and_add(&val->missed_tcp_close, 1);
        break;
    case missed_udp_close:
        __sync_fetch_and_add(&val->missed_udp_close, 1);
        break;
    case udp_send_processed:
        __sync_fetch_and_add(&val->udp_sends_processed, 1);
        break;
    case udp_send_missed:
        __sync_fetch_and_add(&val->udp_sends_missed, 1);
        break;
    }
}

__maybe_unused static __always_inline void sockaddr_to_addr(struct sockaddr *sa, u64 *addr_h, u64 *addr_l, u16 *port, u32 *metadata) {
    if (!sa) {
        return;
    }

    u16 family = 0;
    bpf_probe_read_kernel(&family, sizeof(family), &sa->sa_family);

    struct sockaddr_in *sin;
    struct sockaddr_in6 *sin6;
    switch (family) {
    case AF_INET:
        *metadata |= CONN_V4;
        sin = (struct sockaddr_in *)sa;
        if (addr_l) {
            bpf_probe_read_kernel(addr_l, sizeof(__be32), &(sin->sin_addr.s_addr));
        }
        if (port) {
            bpf_probe_read_kernel(port, sizeof(__be16), &sin->sin_port);
            *port = bpf_ntohs(*port);
        }
        break;
    case AF_INET6:
        *metadata |= CONN_V6;
        sin6 = (struct sockaddr_in6 *)sa;
        if (addr_l && addr_h) {
            bpf_probe_read_kernel(addr_h, sizeof(u64), sin6->sin6_addr.s6_addr);
            bpf_probe_read_kernel(addr_l, sizeof(u64), &(sin6->sin6_addr.s6_addr[8]));
        }
        if (port) {
            bpf_probe_read_kernel(port, sizeof(u16), &sin6->sin6_port);
            *port = bpf_ntohs(*port);
        }
        break;
    default:
        log_debug("ERR(sockaddr_to_addr): invalid family: %u\n", family);
    }
}

#endif // __TRACER_TELEMETRY_H
