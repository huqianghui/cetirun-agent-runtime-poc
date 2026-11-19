// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#include <arpa/inet.h>
#include <nss.h>
#include <resolv.h>
#include <string.h>

int cetirun_test_dns(unsigned short port) {
    if (__nss_configure_lookup("hosts", "dns") != 0 || res_init() != 0) {
        return -1;
    }

    _res.options = RES_INIT | RES_RECURSE | RES_NORELOAD;
    _res.retrans = 30;
    _res.retry = 2;
    _res.nscount = 1;
    memset(_res.nsaddr_list, 0, sizeof(_res.nsaddr_list));
    _res.nsaddr_list[0].sin_family = AF_INET;
    _res.nsaddr_list[0].sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    _res.nsaddr_list[0].sin_port = htons(port);
    return 0;
}
