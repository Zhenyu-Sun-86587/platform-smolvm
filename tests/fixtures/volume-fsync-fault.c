// SPDX-License-Identifier: Apache-2.0
// Inject EIO only after the selected Worker head's data has become read-only.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

int fsync(int fd)
{
    int (*sync_original)(int) = dlsym(RTLD_NEXT, "fsync");
    const char *selector = getenv("VOLUME_TEST_SEAL_FAULT");
    struct stat status;
    if (selector && !fstat(fd, &status) && S_ISREG(status.st_mode) &&
        (status.st_mode & 0777) == 0400) {
        int input = open(selector, O_RDONLY | O_CLOEXEC);
        if (input >= 0) {
            char expected[PATH_MAX], actual[PATH_MAX], proc[64];
            ssize_t count = read(input, expected, sizeof(expected));
            close(input);
            snprintf(proc, sizeof(proc), "/proc/self/fd/%d", fd);
            ssize_t length = readlink(proc, actual, sizeof(actual));
            if (count > 0 && length == count && !memcmp(expected, actual, count)) {
                char observed[PATH_MAX];
                int written = snprintf(observed, sizeof(observed), "%s.observed", selector);
                if (written > 0 && (size_t)written < sizeof(observed)) {
                    int marker = open(observed, O_WRONLY | O_CREAT | O_CLOEXEC, 0600);
                    if (marker >= 0) close(marker);
                }
                errno = EIO;
                return -1;
            }
        }
    }
    return sync_original(fd);
}
