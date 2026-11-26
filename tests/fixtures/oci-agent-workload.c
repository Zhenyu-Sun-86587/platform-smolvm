#define _POSIX_C_SOURCE 200809L
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

static volatile sig_atomic_t stop_signal;

static void remember_stop_signal(int signal)
{
    stop_signal = signal;
}

static int observe(void)
{
    char record[256];
    FILE *progress = fopen("/witness/progress", "r");
    if (progress == NULL)
        return 1;
    char *line = fgets(record, sizeof(record), progress);
    fclose(progress);
    return line == NULL || fputs(line, stdout) == EOF;
}

int main(int argc, char **argv)
{
    if (argc == 2 && strcmp(argv[1], "observe") == 0)
        return observe();
    if (argc != 3 || strcmp(argv[1], "run") != 0)
        return 2;

    if (mkdir("/witness", 0700) != 0 && errno != EEXIST)
        return 1;
    int pid = getpid();
    struct sigaction stop = { .sa_handler = remember_stop_signal };
    if (sigemptyset(&stop.sa_mask) != 0 || sigaction(SIGUSR1, &stop, NULL) != 0)
        return 1;
    int once = open("/witness/started", O_WRONLY | O_CREAT | O_EXCL, 0644);
    if (once < 0) {
        int again = open("/witness/duplicate", O_WRONLY | O_CREAT, 0644);
        if (again >= 0)
            close(again);
        return 1;
    }
    if (dprintf(once, "%s %d\n", argv[2], pid) < 0 || close(once) != 0)
        return 1;

    const struct timespec interval = { .tv_nsec = 100000000 };
    for (unsigned long count = 1;; count++) {
        if (stop_signal || access("/witness/finish", F_OK) == 0) {
            FILE *done = fopen("/witness/exited", "w");
            if (done == NULL)
                return 1;
            int result = fprintf(done, "%s %d %d\n", argv[2], pid, (int)stop_signal);
            return fclose(done) != 0 || result < 0;
        }
        FILE *next = fopen("/witness/next", "w");
        if (next == NULL)
            return 1;
        int result = fprintf(next, "%s %d %lu\n", argv[2], pid, count);
        if (fclose(next) != 0 || result < 0 || rename("/witness/next", "/witness/progress") != 0)
            return 1;
        if (printf("A17_LOG %s %lu\n", argv[2], count) < 0 || fflush(stdout) != 0)
            return 1;
        nanosleep(&interval, NULL);
    }
}
