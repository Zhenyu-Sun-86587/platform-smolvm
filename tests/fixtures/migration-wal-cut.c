/* Stop the test daemon after SQLite writes its first uncommitted WAL page. */
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <signal.h>
#include <stdint.h>
#include <stdlib.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

ssize_t pwrite64(int fd, const void *data, size_t length, off64_t offset)
{
    ssize_t written = syscall(SYS_pwrite64, fd, data, length, offset);
    int saved_errno = errno;
    const char *wal = getenv("MIGRATION_TEST_WAL");
    const char *notify = getenv("MIGRATION_TEST_PIPE");
    struct stat file, expected;
    uint32_t page_size;
    const unsigned char *header = data;
    if (written >= 24 && offset > 32 && wal && notify &&
        !fstat(fd, &file) && !stat(wal, &expected) &&
        file.st_dev == expected.st_dev && file.st_ino == expected.st_ino &&
        pread(fd, &page_size, sizeof(page_size), 8) == sizeof(page_size) &&
        offset == 32 + 24 + (off64_t)ntohl(page_size) &&
        !(header[4] | header[5] | header[6] | header[7])) {
        int pipe_fd = atoi(notify);
        if (write(pipe_fd, "W", 1) == 1)
            raise(SIGSTOP);
    }
    errno = saved_errno;
    return written;
}
