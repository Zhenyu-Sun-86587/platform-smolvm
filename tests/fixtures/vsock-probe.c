#include <sys/socket.h>
#include <linux/vm_sockets.h>
#include <arpa/inet.h>
#include <unistd.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <sys/ioctl.h>

static int write_all(int fd, const char *buf, size_t length)
{
    while (length) {
        ssize_t n = write(fd, buf, length);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) return -1;
        buf += n;
        length -= (size_t)n;
    }
    return 0;
}

static unsigned port_number(const char *port)
{
    char *end;
    unsigned long number = strtoul(port, &end, 10);
    return number >= 1 && number <= 65535 && !*end ? (unsigned)number : 0;
}

static int serve(const char *port, int event_stream)
{
    unsigned number = port_number(port);
    if (!number) return 1;
    signal(SIGPIPE, SIG_IGN);
    int listener = socket(AF_INET, SOCK_STREAM, 0);
    if (listener < 0) return 1;
    struct sockaddr_in address = { .sin_family = AF_INET, .sin_port = htons(number) };
    if (bind(listener, (struct sockaddr *)&address, sizeof(address)) || listen(listener, 8)) {
        perror("listen");
        return 1;
    }
    for (;;) {
        int client = accept(listener, NULL, NULL);
        if (client < 0 && errno == EINTR) continue;
        if (client < 0) return 1;
        if (event_stream) {
            static const char headers[] = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n";
            static const char event[] = "data: finished\n\n";
            char request[512];
            if (read(client, request, sizeof(request)) > 0 &&
                write_all(client, headers, sizeof(headers) - 1) == 0) {
                sleep(20);
                (void)write_all(client, event, sizeof(event) - 1);
            }
        } else {
            (void)write_all(client, "private wake\n", sizeof("private wake\n") - 1);
        }
        close(client);
    }
}

static int udp_server(const char *port)
{
    unsigned number = port_number(port);
    int socket_fd = socket(AF_INET, SOCK_DGRAM, 0);
    struct sockaddr_in address = { .sin_family = AF_INET, .sin_port = htons(number) };
    if (!number || socket_fd < 0 || bind(socket_fd, (struct sockaddr *)&address, sizeof(address))) {
        perror("UDP bind");
        return 1;
    }
    for (;;) {
        char payload[1400];
        socklen_t length = sizeof(address);
        ssize_t count = recvfrom(socket_fd, payload, sizeof(payload), 0, (struct sockaddr *)&address, &length);
        if (count < 0 && errno == EINTR) continue;
        if (count < 0) return 1;
        (void)sendto(socket_fd, payload, (size_t)count, 0, (struct sockaddr *)&address, length);
    }
}

static int udp_exchange(const char *peer, const char *port, const char *size)
{
    unsigned number = port_number(port);
    unsigned count = port_number(size);
    char payload[1400], reply[sizeof(payload)];
    struct sockaddr_in address = { .sin_family = AF_INET, .sin_port = htons(number) };
    if (!number || !count || count > sizeof(payload) || inet_pton(AF_INET, peer, &address.sin_addr) != 1) return 1;
    int socket_fd = socket(AF_INET, SOCK_DGRAM, 0);
    struct timeval timeout = {24, 0};
    if (socket_fd < 0 || setsockopt(socket_fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) ||
        connect(socket_fd, (struct sockaddr *)&address, sizeof(address))) { perror("UDP connection"); return 1; }
    memset(payload, 'A', count);
    if (send(socket_fd, payload, count, 0) != (ssize_t)count) { perror("UDP send"); return 1; }
    ssize_t received = recv(socket_fd, reply, sizeof(reply), 0);
    if (received < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) return 2;
    if (received != (ssize_t)count || memcmp(payload, reply, count)) return 1;
    puts("one datagram returned");
    return 0;
}

int main(int argc, char **argv)
{
    if (argc == 3 && strcmp(argv[1], "--serve") == 0) return serve(argv[2], 0);
    if (argc == 3 && strcmp(argv[1], "--serve-sse") == 0) return serve(argv[2], 1);
    if (argc == 3 && strcmp(argv[1], "--serve-udp") == 0) return udp_server(argv[2]);
    if (argc == 5 && strcmp(argv[1], "--exchange-udp") == 0) return udp_exchange(argv[2], argv[3], argv[4]);
    if (argc == 2 && strcmp(argv[1], "--cid") == 0) {
        unsigned cid;
        int device = open("/dev/vsock", O_RDONLY);
        if (device < 0 || ioctl(device, IOCTL_VM_SOCKETS_GET_LOCAL_CID, &cid) < 0) { perror("get CID"); return 1; }
        printf("%u\n", cid);
        close(device);
        return 0;
    }
    int conn = socket(AF_VSOCK, SOCK_STREAM, 0);
    if (conn < 0) { perror("socket"); return 1; }
    struct timeval timeout = {20, 0};
    setsockopt(conn, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    setsockopt(conn, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
    struct sockaddr_vm host = { .svm_family = AF_VSOCK, .svm_cid = VMADDR_CID_HOST, .svm_port = 6001 };
    if (connect(conn, (struct sockaddr *)&host, sizeof(host))) { perror("connect"); return 1; }
    char buf[4096];
    ssize_t size;
    while ((size = read(STDIN_FILENO, buf, sizeof(buf))) > 0) {
        if (write_all(conn, buf, (size_t)size)) { perror("write request"); return 1; }
    }
    if (size < 0) { perror("read request"); return 1; }
    while ((size = read(conn, buf, sizeof(buf))) > 0) {
        if (write_all(STDOUT_FILENO, buf, (size_t)size)) return 1;
    }
    if (size < 0) { perror("read response"); return 1; }
    return 0;
}
