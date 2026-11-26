#define _POSIX_C_SOURCE 200809L
#include <inttypes.h>
#include <stdint.h>
#include <stdio.h>
#include <time.h>

int main(void)
{
    struct timespec clock[3];
    const clockid_t ids[] = {CLOCK_MONOTONIC, CLOCK_BOOTTIME, CLOCK_REALTIME};

    for (size_t i = 0; i < sizeof(ids) / sizeof(ids[0]); i++) {
        if (clock_gettime(ids[i], &clock[i]) != 0) {
            perror("clock_gettime");
            return 1;
        }
        uint64_t ns = (uint64_t)clock[i].tv_sec * 1000000000 + clock[i].tv_nsec;
        printf("%" PRIu64 "%c", ns, i == 2 ? '\n' : ' ');
    }
    return 0;
}
