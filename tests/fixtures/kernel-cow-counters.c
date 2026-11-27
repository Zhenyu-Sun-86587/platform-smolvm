/* Hardware execution counters for the isolated x86-64 Fork qualification. */
#include <linux/hw_breakpoint.h>
#include <linux/perf_event.h>
#include <stdint.h>
#include <sys/syscall.h>
#include <unistd.h>

int open_fork_counter(pid_t tid, uintptr_t instruction)
{
    struct perf_event_attr attr = {
        .type = PERF_TYPE_BREAKPOINT,
        .size = sizeof(attr),
        .bp_type = HW_BREAKPOINT_X,
        .bp_addr = instruction,
        .bp_len = sizeof(long),
        .inherit = 1,
        .inherit_thread = 1,
        .pinned = 1,
        .exclude_kernel = 1,
        .exclude_hv = 1,
        .exclude_guest = 1,
        .read_format = PERF_FORMAT_TOTAL_TIME_ENABLED | PERF_FORMAT_TOTAL_TIME_RUNNING,
    };
    return (int)syscall(SYS_perf_event_open, &attr, tid, -1, -1, PERF_FLAG_FD_CLOEXEC);
}
