// SPDX-License-Identifier: Apache-2.0
#define _GNU_SOURCE
#include <fcntl.h>
#include <inttypes.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <unistd.h>

#define JOURNAL_SECTOR 8192

enum phase { PREPARED, WRITING_FIRST, SPLIT, WRITING_SECOND, COMMITTED };

static uint64_t salt(unsigned disk)
{
    return disk ? UINT64_C(0x82fe1d16098c0457) : UINT64_C(0x30e659ade84bc213);
}

static int write_disk(int fd, unsigned disk, uint64_t number)
{
    uint64_t sector[512 / sizeof(uint64_t)];
    for (size_t word = 0; word < sizeof(sector) / sizeof(sector[0]); word++)
        sector[word] = number ^ salt(disk) ^ word;
    return pwrite(fd, sector, sizeof(sector), JOURNAL_SECTOR) == sizeof(sector) && fsync(fd) == 0;
}

static int read_disk(int fd, unsigned disk, uint64_t *number)
{
    uint64_t sector[512 / sizeof(uint64_t)];
    if (pread(fd, sector, sizeof(sector), JOURNAL_SECTOR) != sizeof(sector))
        return 0;
    *number = sector[0] ^ salt(disk);
    for (size_t word = 0; word < sizeof(sector) / sizeof(sector[0]); word++)
        if (sector[word] != (*number ^ salt(disk) ^ word))
            return 0;
    return 1;
}

static int report(const char *event, const char *name, const _Atomic uint64_t *state, const int disks[2])
{
    uint64_t counts[2];
    if (!read_disk(disks[0], 0, &counts[0]) || !read_disk(disks[1], 1, &counts[1]))
        return 0;
    uint64_t position = atomic_load_explicit(state, memory_order_relaxed);
    uint64_t number = position >> 3;
    dprintf(1, "%s %d %s %" PRIu64 " %u %u %" PRIu64 " %" PRIu64 "\n",
            event, getpid(), name, number, (unsigned)(position & 7), (unsigned)(number & 1),
            counts[0], counts[1]);
    return 1;
}

static int observe_identity(char name[65], const _Atomic uint64_t *state, const int disks[2])
{
    char actual[65] = {0};
    if (gethostname(actual, sizeof(actual)))
        return 0;
    if (!strcmp(name, actual))
        return 1;
    strcpy(name, actual);
    return report("A09_RESUMED", name, state, disks);
}

static int wait_in_phase(char name[65], const _Atomic uint64_t *state, const int disks[2], unsigned samples)
{
    for (unsigned sample = 0; sample < samples; sample++) {
        if (!observe_identity(name, state, disks))
            return 0;
        usleep(25000);
    }
    return observe_identity(name, state, disks);
}

static int transition(char name[65], _Atomic uint64_t *state, uint64_t number, enum phase phase, const int disks[2])
{
    if (!observe_identity(name, state, disks))
        return 0;
    atomic_store_explicit(state, (number << 3) | phase, memory_order_relaxed);
    if (!report("A09_PHASE", name, state, disks))
        return 0;
    if (phase == PREPARED || phase == SPLIT || phase == COMMITTED)
        return wait_in_phase(name, state, disks, 64);
    return observe_identity(name, state, disks);
}

static int observe_synced_write(char name[65], const _Atomic uint64_t *state, const int disks[2])
{
    return observe_identity(name, state, disks) &&
        report("A09_PHASE", name, state, disks) && wait_in_phase(name, state, disks, 48);
}

int main(void)
{
    const char machine_id[] = "0102030405060708090a0b0c0d0e0f10\n";
    char name[65] = "capture-source";
    if (mount("tmpfs", "/etc", "tmpfs", MS_NOSUID | MS_NODEV, "mode=0755") ||
        sethostname(name, strlen(name)))
        return 1;
    int identity = open("/etc/machine-id", O_WRONLY | O_CREAT | O_EXCL, 0444);
    if (identity < 0 || write(identity, machine_id, 33) != 33 || close(identity))
        return 2;
    int disks[] = {open("/dev/vdb", O_RDWR | O_CLOEXEC), open("/dev/vdc", O_RDWR | O_CLOEXEC)};
    if (disks[0] < 0 || disks[1] < 0 || !write_disk(disks[0], 0, 0) || !write_disk(disks[1], 1, 0))
        return 3;
    _Atomic uint64_t *state = mmap(NULL, sizeof(*state), PROT_READ | PROT_WRITE,
                                   MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (state == MAP_FAILED)
        return 4;
    atomic_init(state, COMMITTED);
    dprintf(1, "PARKED_BLOCK_TICK 0 %d %s fixture 0 0\n", getpid(), name);
    for (uint64_t number = 1; ; number++) {
        unsigned first = number & 1;
        unsigned second = first ^ 1;
        if (!transition(name, state, number, PREPARED, disks) ||
            !transition(name, state, number, WRITING_FIRST, disks) ||
            !write_disk(disks[first], first, number) ||
            !observe_synced_write(name, state, disks) ||
            !transition(name, state, number, SPLIT, disks) ||
            !transition(name, state, number, WRITING_SECOND, disks) ||
            !write_disk(disks[second], second, number) ||
            !observe_synced_write(name, state, disks) ||
            !transition(name, state, number, COMMITTED, disks))
            return 5;
    }
}
