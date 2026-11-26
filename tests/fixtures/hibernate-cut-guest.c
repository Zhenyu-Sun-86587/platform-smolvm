// SPDX-License-Identifier: Apache-2.0
#define _GNU_SOURCE
#include <fcntl.h>
#include <inttypes.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <time.h>
#include <unistd.h>

#define PAGE 4096UL
#define RAM (96UL << 20)
#define FIRST_SECTOR 8192UL

static unsigned char page_byte(size_t page)
{
    return (unsigned char)(1 + page * 37 % 251);
}

static int disclose_pages(unsigned char *memory)
{
    const size_t offsets[] = {0, 4UL << 20, 19UL << 20, 36UL << 20, 60UL << 20, 88UL << 20};
    uint64_t addresses[sizeof(offsets) / sizeof(offsets[0])];
    int fd = open("/proc/self/pagemap", O_RDONLY | O_CLOEXEC);
    if (fd < 0)
        return 0;
    for (size_t index = 0; index < sizeof(offsets) / sizeof(offsets[0]); index++) {
        uintptr_t page = (uintptr_t)(memory + offsets[index]) / PAGE;
        uint64_t entry = 0;
        if (pread(fd, &entry, sizeof(entry), page * sizeof(entry)) != sizeof(entry) || !(entry >> 63))
            return 0;
        uint64_t pfn = entry & ((UINT64_C(1) << 55) - 1);
        if (!pfn)
            return 0;
        addresses[index] = pfn * PAGE;
    }
    close(fd);
    dprintf(1, "A07_GPAS %" PRIu64 " %" PRIu64 " %" PRIu64 " %" PRIu64 " %" PRIu64 " %" PRIu64 "\n",
            addresses[0], addresses[1], addresses[2], addresses[3], addresses[4], addresses[5]);
    return 1;
}

static int verify_cold(const unsigned char *memory)
{
    for (size_t page = 0; page < RAM / PAGE; page++)
        for (size_t index = 0; index < PAGE; index++)
            if (memory[page * PAGE + index] != page_byte(page))
                return 0;
    return 1;
}

static int log_disk(int fd, uint64_t tick, uint64_t previous, uint64_t current)
{
    uint64_t sector[512 / sizeof(uint64_t)];
    off_t offset = FIRST_SECTOR + tick * sizeof(sector);
    if (tick > 1) {
        if (pread(fd, sector, sizeof(sector), offset - sizeof(sector)) != sizeof(sector))
            return 0;
        for (size_t word = 0; word < sizeof(sector) / sizeof(sector[0]); word++)
            if (sector[word] != (previous ^ word))
                return 0;
    }
    for (size_t word = 0; word < sizeof(sector) / sizeof(sector[0]); word++)
        sector[word] = current ^ word;
    return pwrite(fd, sector, sizeof(sector), offset) == sizeof(sector) && fsync(fd) == 0;
}

int main(void)
{
    const char original[] = "0102030405060708090a0b0c0d0e0f10\n";
    if (sysconf(_SC_PAGESIZE) != PAGE ||
        mount("tmpfs", "/etc", "tmpfs", MS_NOSUID | MS_NODEV, "mode=0755") ||
        sethostname("capture-source", 14))
        return 1;
    int identity = open("/etc/machine-id", O_WRONLY | O_CREAT | O_EXCL, 0444);
    if (identity < 0 || write(identity, original, 33) != 33 || close(identity))
        return 2;
    int disks[] = {open("/dev/vdb", O_RDWR | O_CLOEXEC), open("/dev/vdc", O_RDWR | O_CLOEXEC)};
    if (disks[0] < 0 || disks[1] < 0)
        return 3;
    unsigned char *memory = mmap(NULL, RAM, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (memory == MAP_FAILED || madvise(memory, RAM, MADV_NOHUGEPAGE))
        return 4;
    for (size_t page = 0; page < RAM / PAGE; page++)
        memset(memory + page * PAGE, page_byte(page), PAGE);
    if (!disclose_pages(memory))
        return 5;

    time_t child_started = 0;
    int child_read = 0;
    uint64_t previous[] = {0, 0};
    for (uint64_t tick = 1;; tick++) {
        char name[65] = {0};
        if (gethostname(name, sizeof(name)))
            return 6;
        int child = !strcmp(name, "a07-child");
        if (child && !child_read) {
            struct timespec time = {0};
            if (clock_gettime(CLOCK_MONOTONIC, &time))
                return 7;
            if (!child_started) {
                child_started = time.tv_sec;
                dprintf(1, "A07_CHILD_WAIT %d\n", getpid());
            }
            if (time.tv_sec - child_started >= 50) {
                if (!verify_cold(memory))
                    return 8;
                child_read = 1;
                dprintf(1, "A07_CHILD_COLD_READ %d %lu\n", getpid(), RAM);
            }
        }

        uint64_t stamp = tick ^ (child ? UINT64_C(0x3c5a000000000000) : 0);
        uint64_t words[] = {stamp ^ UINT64_C(0x0123456789abcdef), stamp ^ UINT64_C(0xfedcba9876543210)};
        for (int disk = 0; disk < 2; disk++) {
            if (!log_disk(disks[disk], tick, previous[disk], words[disk]))
                return 9;
            previous[disk] = words[disk];
        }
        dprintf(1, "PARKED_BLOCK_TICK %" PRIu64 " %d %s %016" PRIx64 " %016" PRIx64 "\n",
                tick, getpid(), name, words[0], words[1]);
        usleep(220000);
    }
}
