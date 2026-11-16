/* Inspect inheritance before opening a report or any other application file. */
#define _POSIX_C_SOURCE 200809L
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/stat.h>
#include <unistd.h>

extern char **environ;

struct observed_fd {
    int fd;
    int flags;
    struct stat info;
    struct observed_fd *next;
};

int main(void) {
    struct stat lease;
    int have_lease = fstat(3, &lease) == 0;
    errno = 0;
    ssize_t wrote = write(3, "x", 1);
    int write_ebadf = wrote == -1 && errno == EBADF;
    DIR *fds = opendir("/dev/fd");
    if (fds == NULL) return 1;
    int inventory_fd = dirfd(fds);
    if (inventory_fd < 0) return 1;
    struct observed_fd *observed = NULL;
    for (;;) {
        errno = 0;
        struct dirent *entry = readdir(fds);
        if (entry == NULL) {
            if (errno != 0) return 1;
            break;
        }
        char *end;
        errno = 0;
        long number = strtol(entry->d_name, &end, 10);
        if (end == entry->d_name || *end != '\0') continue;
        if (errno != 0 || number < 0 || number > INT_MAX) return 1;
        int fd = (int)number;
        /* Exclude only stdio/lease and the exact handle opened above. */
        if (fd <= 3 || fd == inventory_fd) continue;
        struct observed_fd *item = malloc(sizeof(*item));
        if (item == NULL) return 1;
        item->fd = fd;
        item->flags = fcntl(fd, F_GETFD);
        if (item->flags == -1 || fstat(fd, &item->info) == -1) return 1;
        item->next = observed;
        observed = item;
    }
    if (closedir(fds) != 0) return 1;
    /* No report descriptor was present during the complete inventory. */
    FILE *report = fopen("fd-report", "w");
    if (report == NULL) return 1;
    if (have_lease) {
        fprintf(report, "fd3 %ju:%ju regular=%s write_ebadf=%s\n",
                (uintmax_t)lease.st_dev, (uintmax_t)lease.st_ino,
                S_ISREG(lease.st_mode) ? "true" : "false",
                write_ebadf ? "true" : "false");
    } else {
        fprintf(report, "fd3 missing\n");
    }
    while (observed != NULL) {
        struct observed_fd *next = observed->next;
        fprintf(report, "open %d %ju:%ju mode=%jo flags=%d\n", observed->fd,
                (uintmax_t)observed->info.st_dev, (uintmax_t)observed->info.st_ino,
                (uintmax_t)observed->info.st_mode, observed->flags);
        free(observed);
        observed = next;
    }
    for (char **kv = environ; *kv != NULL; kv++) {
        char *equals = *kv;
        while (*equals != '\0' && *equals != '=') equals++;
        if (equals - *kv > INT_MAX) return 1;
        fprintf(report, "env %.*s\n", (int)(equals - *kv), *kv);
    }
    if (ferror(report) || fclose(report) != 0) return 1;
    char buffer[4096];
    for (;;) {
        ssize_t n = read(0, buffer, sizeof(buffer));
        if (n == -1 && errno == EINTR) continue;
        if (n == -1) return 1;
        if (n == 0) return 0;
    }
}
