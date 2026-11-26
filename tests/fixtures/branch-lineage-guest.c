// SPDX-License-Identifier: Apache-2.0
#define _GNU_SOURCE
#include <fcntl.h>
#include <inttypes.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <unistd.h>

#define PAGE_WORDS (4096UL / sizeof(uint64_t))
#define PAGES 1024UL
#define BLOCK_OFFSET 4096

static const char *const branches[] = {
    "capture-source", "a03-left", "a03-right", "a03-middle", "a03-leaf",
};
static const size_t parents[] = {0, 0, 0, 1, 3};

static uint64_t marker(size_t branch, uint64_t tick)
{
    return UINT64_C(0x34ff000000000000) ^ ((uint64_t)branch << 48) ^ tick;
}

static uint64_t disk_marker(uint64_t stamp, int disk)
{
    return disk ? stamp ^ UINT64_C(0x0123456789abcdef) : stamp;
}

static void write_memory(uint64_t *pages, uint64_t stamp)
{
    for (size_t page = 0; page < PAGES; page++) {
        pages[page * PAGE_WORDS] = stamp ^ page;
        pages[(page + 1) * PAGE_WORDS - 1] = ~(stamp ^ page);
    }
}

static int check_memory(const uint64_t *pages, uint64_t stamp)
{
    for (size_t page = 0; page < PAGES; page++)
        if (pages[page * PAGE_WORDS] != (stamp ^ page) ||
            pages[(page + 1) * PAGE_WORDS - 1] != ~(stamp ^ page))
            return 0;
    return 1;
}

static int write_disk(int fd, uint64_t stamp)
{
    uint64_t sector[512 / sizeof(uint64_t)];
    for (size_t index = 0; index < sizeof(sector) / sizeof(sector[0]); index++)
        sector[index] = stamp ^ index;
    return pwrite(fd, sector, sizeof(sector), BLOCK_OFFSET) == sizeof(sector) && fsync(fd) == 0;
}

static int check_disk(int fd, uint64_t stamp)
{
    uint64_t sector[512 / sizeof(uint64_t)];
    if (pread(fd, sector, sizeof(sector), BLOCK_OFFSET) != sizeof(sector))
        return 0;
    for (size_t index = 0; index < sizeof(sector) / sizeof(sector[0]); index++)
        if (sector[index] != (stamp ^ index))
            return 0;
    return 1;
}

int main(void)
{
    const char identity[] = "0102030405060708090a0b0c0d0e0f10\n";
    if (mount("tmpfs", "/etc", "tmpfs", MS_NOSUID | MS_NODEV, "mode=0755") ||
        sethostname(branches[0], strlen(branches[0])))
        return 1;
    int machine_id = open("/etc/machine-id", O_WRONLY | O_CREAT | O_EXCL, 0444);
    if (machine_id < 0 || write(machine_id, identity, 33) != 33 || close(machine_id))
        return 2;
    int drives[] = {open("/dev/vdb", O_RDWR | O_CLOEXEC), open("/dev/vdc", O_RDWR | O_CLOEXEC)};
    if (drives[0] < 0 || drives[1] < 0)
        return 3;

    uint64_t *pages = mmap(NULL, PAGES * PAGE_WORDS * sizeof(*pages), PROT_READ | PROT_WRITE,
                           MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (pages == MAP_FAILED)
        return 4;

    size_t branch = 0;
    uint64_t previous = marker(branch, 0);
    write_memory(pages, previous);
    if (!write_disk(drives[0], disk_marker(previous, 0)) || !write_disk(drives[1], disk_marker(previous, 1)))
        return 5;

    for (uint64_t tick = 1;; ++tick) {
        char hostname[65] = {0};
        if (gethostname(hostname, sizeof(hostname)))
            return 6;
        size_t next = 0;
        while (next < sizeof(branches) / sizeof(branches[0]) && strcmp(hostname, branches[next]))
            ++next;
        if (next == sizeof(branches) / sizeof(branches[0]))
            return 7;
        if (!check_memory(pages, previous) ||
            !check_disk(drives[0], disk_marker(previous, 0)) ||
            !check_disk(drives[1], disk_marker(previous, 1))) {
            dprintf(1, "A03_MISMATCH %s %" PRIu64 "\n", hostname, tick);
            return 8;
        }
        if (next != branch) {
            if (parents[next] != branch)
                return 9;
            dprintf(1, "A03_BRANCH %s %s %d %" PRIu64 "\n", branches[next], branches[branch], getpid(), tick - 1);
            branch = next;
        }
        uint64_t current = marker(branch, tick);
        write_memory(pages, current);
        for (int disk = 0; disk < 2; ++disk)
            if (!write_disk(drives[disk], disk_marker(current, disk)))
                return 10;
        previous = current;
        dprintf(1, "PARKED_BLOCK_TICK %" PRIu64 " %d %s %016" PRIx64 " %016" PRIx64 " %016" PRIx64 "\n",
                tick, getpid(), hostname, current, disk_marker(current, 0), disk_marker(current, 1));
        usleep(260000);
    }
}
