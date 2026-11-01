/* Stand-in Windows game server for e2e/gameserver-proton.sh.
 *
 * A real Windows (PE) executable, cross-compiled with mingw-w64, so running it
 * proves Proton/Wine executes a Windows binary through the driver. Listens on a
 * UDP port and answers every datagram with "WINPONG " + the payload. That
 * prefix cannot come from anything but this binary.
 *
 *   x86_64-w64-mingw32-gcc -O2 -o server.exe winserver.c -lws2_32
 */
#include <winsock2.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

int main(int argc, char **argv) {
    int port = argc > 1 ? atoi(argv[1]) : 27098;
    WSADATA wsa;
    if (WSAStartup(MAKEWORD(2, 2), &wsa) != 0) {
        fprintf(stderr, "WSAStartup failed\n");
        return 2;
    }

    SOCKET s = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof addr);
    addr.sin_family = AF_INET;
    addr.sin_port = htons((unsigned short)port);
    addr.sin_addr.s_addr = inet_addr("127.0.0.1");
    if (bind(s, (struct sockaddr *)&addr, sizeof addr) != 0) {
        fprintf(stderr, "bind failed: %d\n", WSAGetLastError());
        return 3;
    }

    printf("windows server listening on udp/%d\n", port);
    fflush(stdout);

    for (;;) {
        char buf[1024];
        struct sockaddr_in from;
        int fromlen = sizeof from;
        int n = recvfrom(s, buf, sizeof buf, 0, (struct sockaddr *)&from, &fromlen);
        if (n < 0) {
            continue;
        }
        char out[1100];
        memcpy(out, "WINPONG ", 8);
        memcpy(out + 8, buf, (size_t)n);
        sendto(s, out, n + 8, 0, (struct sockaddr *)&from, fromlen);
    }
}
