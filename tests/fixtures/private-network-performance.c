/* Measure a small, fixed amount of traffic between two owned private Guests. */
#define _POSIX_C_SOURCE 200809L
#include <arpa/inet.h>
#include <err.h>
#include <errno.h>
#include <inttypes.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

enum {
    Echo = 1,
    Bulk = 2,
    FrameBytes = 32,
    Warmups = 4,
    EchoSamples = 48,
    BulkSamples = 3,
    BlockBytes = 64 * 1024,
    BulkBytes = 4 * 1024 * 1024,
};

static uint64_t monotonic_ns(void)
{
    struct timespec now;
    if (clock_gettime(CLOCK_MONOTONIC, &now) < 0)
        err(1, "clock_gettime");
    return (uint64_t)now.tv_sec * 1000000000 + (uint64_t)now.tv_nsec;
}

static void receive(int fd, void *buffer, size_t length)
{
    unsigned char *next = buffer;
    while (length) {
        ssize_t count = read(fd, next, length);
        if (count < 0 && errno == EINTR)
            continue;
        if (count < 0)
            err(1, "socket read");
        if (count == 0)
            errx(1, "unexpected EOF");
        next += count;
        length -= (size_t)count;
    }
}

static void transmit(int fd, const void *buffer, size_t length)
{
    const unsigned char *next = buffer;
    while (length) {
        ssize_t count = write(fd, next, length);
        if (count < 0 && errno == EINTR)
            continue;
        if (count <= 0)
            err(1, "socket write");
        next += count;
        length -= (size_t)count;
    }
}

static void enable_option(int fd, int level, int option)
{
    int one = 1;
    if (setsockopt(fd, level, option, &one, sizeof(one)) < 0)
        err(1, "setsockopt");
}

static void make_block(void *buffer, uint32_t sample, uint32_t index)
{
    const uint32_t positions[] = { htonl(sample), htonl(index) };
    memset(buffer, 'b', BlockBytes);
    memcpy(buffer, positions, sizeof(positions));
}

static void serve(uint16_t port)
{
    int listener = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (listener < 0)
        err(1, "listener socket");
    enable_option(listener, SOL_SOCKET, SO_REUSEADDR);
    struct sockaddr_in local = { .sin_family = AF_INET, .sin_port = htons(port) };
    if (bind(listener, (struct sockaddr *)&local, sizeof(local)) < 0 || listen(listener, 4) < 0)
        err(1, "listen");
    unsigned char buffer[BlockBytes], expected[BlockBytes];
    for (;;) {
        struct sockaddr_in from;
        socklen_t from_length = sizeof(from);
        int connection = accept(listener, (struct sockaddr *)&from, &from_length);
        if (connection < 0)
            err(1, "accept");
        alarm(18);
        enable_option(connection, IPPROTO_TCP, TCP_NODELAY);
        uint32_t header[3];
        receive(connection, header, sizeof(header));
        uint32_t operation = ntohl(header[0]);
        uint32_t size = ntohl(header[1]);
        uint32_t sample = ntohl(header[2]);
        transmit(connection, &from.sin_addr.s_addr, sizeof(from.sin_addr.s_addr));
        if (operation == Echo && size == Warmups + EchoSamples) {
            for (uint32_t i = 0; i < size; ++i) {
                receive(connection, buffer, FrameBytes);
                transmit(connection, buffer, FrameBytes);
            }
        } else if (operation == Bulk && size == BulkBytes && sample < BulkSamples) {
            for (uint32_t i = 0; i < BulkBytes / BlockBytes; ++i) {
                receive(connection, buffer, BlockBytes);
                make_block(expected, sample, i);
                if (memcmp(buffer, expected, BlockBytes) != 0)
                    errx(1, "payload differs at block %" PRIu32, i);
            }
            uint32_t verified = htonl(sample + 1);
            transmit(connection, &verified, sizeof(verified));
        } else {
            errx(1, "unexpected fixture request");
        }
        close(connection);
        alarm(0);
    }
}

static int connect_case(struct sockaddr_in target, struct in_addr source,
                        uint32_t operation, uint32_t size, uint32_t sample)
{
    int fd = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (fd < 0)
        err(1, "client socket");
    enable_option(fd, IPPROTO_TCP, TCP_NODELAY);
    if (connect(fd, (struct sockaddr *)&target, sizeof(target)) < 0)
        err(1, "connect");
    const uint32_t header[] = { htonl(operation), htonl(size), htonl(sample) };
    transmit(fd, header, sizeof(header));
    struct in_addr observed;
    receive(fd, &observed.s_addr, sizeof(observed.s_addr));
    if (observed.s_addr != source.s_addr)
        errx(1, "peer saw unexpected source %s", inet_ntoa(observed));
    return fd;
}

static void echo_short_frames(struct sockaddr_in target, struct in_addr source)
{
    int fd = connect_case(target, source, Echo, Warmups + EchoSamples, 0);
    unsigned char frame[FrameBytes], reply[FrameBytes];
    memset(frame, 'r', sizeof(frame));
    uint64_t durations[EchoSamples];
    for (uint32_t i = 0; i < Warmups + EchoSamples; ++i) {
        uint32_t sequence = htonl(i);
        memcpy(frame, &sequence, sizeof(sequence));
        uint64_t start = monotonic_ns();
        transmit(fd, frame, sizeof(frame));
        receive(fd, reply, sizeof(reply));
        uint64_t elapsed = monotonic_ns() - start;
        if (memcmp(frame, reply, sizeof(frame)) != 0)
            errx(1, "echo differed from the sent frame");
        if (i >= Warmups)
            durations[i - Warmups] = elapsed;
    }
    close(fd);
    printf("{\"frame_bytes\":%d,\"warmup\":%d,\"rtt_ns\":[", FrameBytes, Warmups);
    for (int i = 0; i < EchoSamples; ++i)
        printf("%s%" PRIu64, i ? "," : "", durations[i]);
    puts("]}");
}

static void send_bulk_payloads(struct sockaddr_in target, struct in_addr source)
{
    static unsigned char payload[BulkBytes];
    uint64_t durations[BulkSamples];
    for (uint32_t sample = 0; sample < BulkSamples; ++sample) {
        for (uint32_t block = 0; block < BulkBytes / BlockBytes; ++block)
            make_block(payload + block * BlockBytes, sample, block);
        int fd = connect_case(target, source, Bulk, BulkBytes, sample);
        uint64_t start = monotonic_ns();
        transmit(fd, payload, sizeof(payload));
        uint32_t verified;
        receive(fd, &verified, sizeof(verified));
        durations[sample] = monotonic_ns() - start;
        if (ntohl(verified) != sample + 1)
            errx(1, "receiver acknowledged a different sample");
        close(fd);
    }
    fputs("{\"transfers\":[", stdout);
    for (int i = 0; i < BulkSamples; ++i)
        printf("%s{\"sequence\":%d,\"bytes\":%d,\"elapsed_ns\":%" PRIu64 ",\"verified\":true}",
               i ? "," : "", i, BulkBytes, durations[i]);
    puts("]}");
}

int main(int argc, char **argv)
{
    if (argc != 3 && argc != 5)
        errx(1, "usage: %s server port | rtt|bulk port destination expected-source", argv[0]);
    char *tail;
    unsigned long port = strtoul(argv[2], &tail, 10);
    if (tail == argv[2] || *tail || port == 0 || port > UINT16_MAX)
        errx(1, "invalid port");
    if (argc == 3 && strcmp(argv[1], "server") == 0) {
        serve((uint16_t)port);
        return 0;
    }
    if (argc != 5)
        errx(1, "client needs a destination and expected source");
    struct sockaddr_in target = { .sin_family = AF_INET, .sin_port = htons((uint16_t)port) };
    struct in_addr source;
    if (inet_pton(AF_INET, argv[3], &target.sin_addr) != 1 || inet_pton(AF_INET, argv[4], &source) != 1)
        errx(1, "expected IPv4 addresses");
    alarm(40);
    if (strcmp(argv[1], "rtt") == 0)
        echo_short_frames(target, source);
    else if (strcmp(argv[1], "bulk") == 0)
        send_bulk_payloads(target, source);
    else
        errx(1, "unknown fixture mode");
    return 0;
}
