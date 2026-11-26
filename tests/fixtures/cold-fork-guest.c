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
#define RAM (64UL << 20)
#define CHANGE (8UL << 20)
#define ORIGINAL_SECTOR 8192UL
#define SECOND_SECTOR 24576UL

static unsigned char pattern(size_t page, unsigned generation)
{
    return (unsigned char)(1 + (page * 37 + generation * 97) % 251);
}

static int write_marker(int fd, off_t address, unsigned char value)
{
    unsigned char sector[512];
    memset(sector, value, sizeof(sector));
    return pwrite(fd, sector, sizeof(sector), address) == sizeof(sector) && !fsync(fd);
}

static int check_marker(int fd, off_t address, unsigned char value)
{
    unsigned char sector[512];
    if (pread(fd, sector, sizeof(sector), address) != sizeof(sector))
        return 0;
    for (size_t index = 0; index < sizeof(sector); index++)
        if (sector[index] != value)
            return 0;
    return 1;
}

static int cold_gpas(unsigned char *memory)
{
    const size_t offsets[] = {0, 4UL << 20, 12UL << 20, 24UL << 20, 40UL << 20, 52UL << 20};
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
    dprintf(1, "A02_COLD_GPAS %" PRIu64 " %" PRIu64 " %" PRIu64 " %" PRIu64 " %" PRIu64 " %" PRIu64 "\n",
            addresses[0], addresses[1], addresses[2], addresses[3], addresses[4], addresses[5]);
    return 1;
}

static uint64_t seconds_since_boot(void)
{
    struct timespec current = {0};
    if (clock_gettime(CLOCK_MONOTONIC, &current))
        return 0;
    return (uint64_t)current.tv_sec;
}

int main(void)
{
    const char original_identity[] = "0102030405060708090a0b0c0d0e0f10\n";
    if (sysconf(_SC_PAGESIZE) != PAGE ||
        mount("tmpfs", "/etc", "tmpfs", MS_NOSUID | MS_NODEV, "mode=0755") ||
        sethostname("capture-source", 14))
        return 1;
    int identity = open("/etc/machine-id", O_WRONLY | O_CREAT | O_EXCL, 0444);
    if (identity < 0 || write(identity, original_identity, 33) != 33 || close(identity))
        return 2;

    int disks[] = {open("/dev/vdb", O_RDWR | O_CLOEXEC), open("/dev/vdc", O_RDWR | O_CLOEXEC)};
    off_t positions[] = {ORIGINAL_SECTOR + 37, ORIGINAL_SECTOR + 38};
    for (int index = 0; index < 2; index++)
        if (disks[index] < 0 || !write_marker(disks[index], ORIGINAL_SECTOR, (unsigned char)(0xa0 + index)) ||
            lseek(disks[index], positions[index], SEEK_SET) != positions[index])
            return 3;

    unsigned char *memory = mmap(NULL, RAM, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (memory == MAP_FAILED || madvise(memory, RAM, MADV_NOHUGEPAGE))
        return 4;
    for (size_t page = 0; page < RAM / PAGE; page++)
        memset(memory + page * PAGE, pattern(page, 0), PAGE);
    if (!cold_gpas(memory))
        return 5;

    int layer_changed = 0, cold_read = 0, descendant_seen = 0;
    uint64_t armed = 0;
    for (uint64_t tick = 1;; tick++) {
        char name[65] = {0};
        if (gethostname(name, sizeof(name)))
            return 6;
        if (!layer_changed && !strcmp(name, "backing-first")) {
            for (size_t page = (RAM - CHANGE) / PAGE; page < RAM / PAGE; page++)
                memset(memory + page * PAGE, pattern(page, 1), PAGE);
            for (int index = 0; index < 2; index++)
                if (!write_marker(disks[index], SECOND_SECTOR, (unsigned char)(0xc0 + index)))
                    return 7;
            layer_changed = 1;
            dprintf(1, "A02_LAYER_ONE %d %lu %lu\n", getpid(), RAM - CHANGE, CHANGE);
        }
        if (!strcmp(name, "cold-leaf") && !cold_read) {
            if (!armed) {
                armed = seconds_since_boot();
                if (!armed)
                    return 8;
                dprintf(1, "A02_COLD_LEAF_ARMED %d\n", getpid());
            }
            if (seconds_since_boot() - armed >= 25) {
                if (!layer_changed)
                    return 9;
                for (size_t page = 0; page < RAM / PAGE; page++) {
                    unsigned char expected = pattern(page, (page * PAGE >= RAM - CHANGE));
                    for (size_t byte = 0; byte < PAGE; byte++)
                        if (memory[page * PAGE + byte] != expected)
                            return 10;
                }
                for (int index = 0; index < 2; index++) {
                    unsigned char bytes[17];
                    if (!check_marker(disks[index], ORIGINAL_SECTOR, (unsigned char)(0xa0 + index)) ||
                        !check_marker(disks[index], SECOND_SECTOR, (unsigned char)(0xc0 + index)) ||
                        lseek(disks[index], 0, SEEK_CUR) != positions[index] ||
                        read(disks[index], bytes, sizeof(bytes)) != sizeof(bytes))
                        return 11;
                    for (size_t byte = 0; byte < sizeof(bytes); byte++)
                        if (bytes[byte] != (unsigned char)(0xa0 + index))
                            return 12;
                    off_t after = lseek(disks[index], 0, SEEK_CUR);
                    if (after != positions[index] + (off_t)sizeof(bytes))
                        return 13;
                    dprintf(1, "A02_DISK %d %jd %jd %u %u\n", index, (intmax_t)positions[index], (intmax_t)after,
                            (unsigned)(0xa0 + index), (unsigned)(0xc0 + index));
                }
                cold_read = 1;
                dprintf(1, "A02_COLD_READ %d %lu\n", getpid(), RAM);
            }
        }
        if (!descendant_seen && !strcmp(name, "cold-descendant")) {
            if (!cold_read)
                return 14;
            uint64_t checksum = 0;
            for (size_t page = 0; page < RAM / PAGE; page++) {
                unsigned char byte = memory[page * PAGE];
                if (byte != pattern(page, (page * PAGE >= RAM - CHANGE)))
                    return 15;
                checksum += byte;
            }
            for (int index = 0; index < 2; index++)
                if (!check_marker(disks[index], ORIGINAL_SECTOR, (unsigned char)(0xa0 + index)) ||
                    !check_marker(disks[index], SECOND_SECTOR, (unsigned char)(0xc0 + index)) ||
                    lseek(disks[index], 0, SEEK_CUR) != positions[index] + 17)
                    return 16;
            dprintf(1, "A02_DESCENDANT %d %" PRIu64 "\n", getpid(), checksum);
            descendant_seen = 1;
        }
        dprintf(1, "PARKED_BLOCK_TICK %" PRIu64 " %d %s fixture 0 0\n", tick, getpid(), name);
        usleep(200000);
    }
}
