#include <sys/socket.h>
#include <linux/vm_sockets.h>
#include <unistd.h>
#include <stdio.h>
#include <string.h>
#include <errno.h>
#include <fcntl.h>
#include <sys/ioctl.h>

static int write_all(int fd, const char *buf, size_t length)
{
    while (length) {
        ssize_t n = write(fd, buf, length);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) return -1;
        buf += n;
        length -= (size_t)n;
    }
    return 0;
}

int main(int argc, char **argv)
{
    if (argc == 2 && strcmp(argv[1], "--cid") == 0) {
        unsigned cid;
        int device = open("/dev/vsock", O_RDONLY);
        if (device < 0 || ioctl(device, IOCTL_VM_SOCKETS_GET_LOCAL_CID, &cid) < 0) { perror("get CID"); return 1; }
        printf("%u\n", cid);
        close(device);
        return 0;
    }
    int conn = socket(AF_VSOCK, SOCK_STREAM, 0);
    if (conn < 0) { perror("socket"); return 1; }
    struct timeval timeout = {20, 0};
    setsockopt(conn, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    setsockopt(conn, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
    struct sockaddr_vm host = { .svm_family = AF_VSOCK, .svm_cid = VMADDR_CID_HOST, .svm_port = 6001 };
    if (connect(conn, (struct sockaddr *)&host, sizeof(host))) { perror("connect"); return 1; }
    char buf[4096];
    ssize_t size;
    while ((size = read(STDIN_FILENO, buf, sizeof(buf))) > 0) {
        if (write_all(conn, buf, (size_t)size)) { perror("write request"); return 1; }
    }
    if (size < 0) { perror("read request"); return 1; }
    while ((size = read(conn, buf, sizeof(buf))) > 0) {
        if (write_all(STDOUT_FILENO, buf, (size_t)size)) return 1;
    }
    if (size < 0) { perror("read response"); return 1; }
    return 0;
}
