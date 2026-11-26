#define _GNU_SOURCE
#include <dlfcn.h>
#include <fcntl.h>
#include <limits.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

// Pause the isolated Worker before unlinking the one armed object data file.
int unlink(const char *path) {
    int (*next_unlink)(const char *) = dlsym(RTLD_NEXT, "unlink");
    const char *target_file = getenv("FIXTURE_GC_TARGET");
    const char *reached_file = getenv("FIXTURE_GC_REACHED");
    int parent_fd;
    int parsed = 0;
    if (target_file != NULL && reached_file != NULL &&
        sscanf(path, "/proc/self/fd/%d/data%n", &parent_fd, &parsed) == 1 &&
        parsed > 0 && path[parsed] == '\0') {
        char expected[PATH_MAX];
        int control = open(target_file, O_RDONLY | O_CLOEXEC);
        if (control >= 0) {
            ssize_t bytes = read(control, expected, sizeof(expected) - 1);
            close(control);
            if (bytes > 0) {
                expected[bytes] = '\0';
                char fd_path[64];
                snprintf(fd_path, sizeof(fd_path), "/proc/self/fd/%d", parent_fd);
                char actual[PATH_MAX];
                ssize_t length = readlink(fd_path, actual, sizeof(actual) - sizeof("/data"));
                if (length > 0) {
                    memcpy(actual + length, "/data", sizeof("/data"));
                    if (strcmp(actual, expected) == 0) {
                        int reached = open(reached_file, O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0600);
                        if (reached >= 0) {
                            close(reached);
                            kill(getpid(), SIGSTOP);
                        }
                    }
                }
            }
        }
    }
    return next_unlink(path);
}
