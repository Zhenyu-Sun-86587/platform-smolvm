#define _GNU_SOURCE
#include <cpuid.h>
#include <dirent.h>
#include <fcntl.h>
#include <inttypes.h>
#include <linux/kvm.h>
#include <sched.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/utsname.h>
#include <unistd.h>

static int kvm_probe(void)
{
    const struct {
        const char *name;
        int cap;
    } capabilities[] = {
        {"KVM_CAP_IOEVENTFD", KVM_CAP_IOEVENTFD},
        {"KVM_CAP_IRQCHIP", KVM_CAP_IRQCHIP},
        {"KVM_CAP_IRQFD", KVM_CAP_IRQFD},
        {"KVM_CAP_SET_TSS_ADDR", KVM_CAP_SET_TSS_ADDR},
        {"KVM_CAP_USER_MEMORY", KVM_CAP_USER_MEMORY},
    };
    int fd = open("/dev/kvm", O_RDWR | O_CLOEXEC);
    if (fd < 0) {
        perror("/dev/kvm");
        return 1;
    }
    int version = ioctl(fd, KVM_GET_API_VERSION, 0);
    if (version != KVM_API_VERSION) {
        fprintf(stderr, "unexpected KVM API version %d\n", version);
        return 1;
    }
    printf("{\"version\":%d,\"extensions\":{", version);
    for (size_t i = 0; i < sizeof(capabilities) / sizeof(capabilities[0]); i++) {
        int value = ioctl(fd, KVM_CHECK_EXTENSION, capabilities[i].cap);
        if (value <= 0) {
            fprintf(stderr, "missing %s\n", capabilities[i].name);
            return 1;
        }
        printf("%s\"%s\":%d", i ? "," : "", capabilities[i].name, value);
    }
    enum { max_msrs = 1024 };
    struct kvm_msr_list *msrs = calloc(1, sizeof(*msrs) + max_msrs * sizeof(msrs->indices[0]));
    if (!msrs) {
        perror("allocate MSR list");
        return 1;
    }
    msrs->nmsrs = max_msrs;
    if (ioctl(fd, KVM_GET_MSR_INDEX_LIST, msrs) < 0) {
        perror("KVM_GET_MSR_INDEX_LIST");
        return 1;
    }
    printf("},\"msr_indices\":[");
    for (uint32_t i = 0; i < msrs->nmsrs; i++)
        printf("%s\"%08" PRIx32 "\"", i ? "," : "", msrs->indices[i]);
    printf("]}\n");
    free(msrs);
    close(fd);
    return 0;
}

static int guest_probe(void)
{
    const unsigned int leaves[][2] = {
        {0, 0}, {1, 0}, {7, 0}, {7, 1}, {0xd, 0}, {0xd, 1},
        {0x40000000, 0}, {0x40000001, 0},
        {0x80000000, 0}, {0x80000001, 0}, {0x80000007, 0}, {0x80000008, 0},
    };
    struct utsname system;
    if (uname(&system) < 0) {
        perror("uname");
        return 1;
    }
    FILE *clock = fopen("/sys/devices/system/clocksource/clocksource0/current_clocksource", "r");
    char clocksource[64];
    if (!clock || fscanf(clock, "%63[a-zA-Z0-9_-]", clocksource) != 1) {
        fprintf(stderr, "cannot read the guest clocksource\n");
        return 1;
    }
    fclose(clock);

    cpu_set_t cpu;
    CPU_ZERO(&cpu);
    CPU_SET(0, &cpu);
    if (sched_setaffinity(0, sizeof(cpu), &cpu) < 0) {
        perror("sched_setaffinity");
        return 1;
    }
    unsigned int eax, ebx, ecx, edx;
    __cpuid(1, eax, ebx, ecx, edx);
    if (!(ecx & bit_OSXSAVE)) {
        fprintf(stderr, "guest CPUID does not advertise OSXSAVE\n");
        return 1;
    }
    __asm__ volatile("xgetbv" : "=a"(eax), "=d"(edx) : "c"(0));
    printf("{\"kernel_release\":\"%s\",\"clocksource\":\"%s\",\"xcr0\":\"%08x%08x\",\"cpuid\":[",
           system.release, clocksource, edx, eax);
    for (size_t i = 0; i < sizeof(leaves) / sizeof(leaves[0]); i++) {
        __cpuid_count(leaves[i][0], leaves[i][1], eax, ebx, ecx, edx);
        printf("%s{\"leaf\":\"%08x\",\"subleaf\":%u,\"eax\":\"%08x\","
               "\"ebx\":\"%08x\",\"ecx\":\"%08x\",\"edx\":\"%08x\"}",
               i ? "," : "", leaves[i][0], leaves[i][1], eax, ebx, ecx, edx);
    }
    const char *base = "/sys/bus/virtio/devices";
    DIR *devices = opendir(base);
    if (!devices) {
        perror(base);
        return 1;
    }
    printf("],\"virtio_devices\":[");
    struct dirent *entry;
    size_t count = 0;
    while ((entry = readdir(devices)) != NULL) {
        unsigned int slot, id;
        char path[512];
        if (sscanf(entry->d_name, "virtio%u", &slot) != 1)
            continue;
        snprintf(path, sizeof(path), "%s/%s/device", base, entry->d_name);
        FILE *device = fopen(path, "r");
        if (!device || fscanf(device, "%x", &id) != 1) {
            fprintf(stderr, "cannot read the ID of %s\n", entry->d_name);
            return 1;
        }
        fclose(device);
        printf("%s{\"slot\":%u,\"device_id\":%u}", count++ ? "," : "", slot, id);
    }
    closedir(devices);
    printf("]}\n");
    return 0;
}

int main(int argc, char **argv)
{
    if (argc == 2 && strcmp(argv[1], "host") == 0)
        return kvm_probe();
    if (argc == 2 && strcmp(argv[1], "guest") == 0)
        return guest_probe();
    fprintf(stderr, "usage: %s host|guest\n", argv[0]);
    return 2;
}
