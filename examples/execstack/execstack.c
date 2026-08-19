#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/personality.h>
#include <unistd.h>

/* READ_IMPLIES_EXEC is applied by the ELF loader at execve(2), not when the
 * syscall returns.  Calling personality() from inside a running process is
 * therefore too late: the call SUCCEEDS, and the heap stays rw-p, because the
 * brk heap was already mapped before main() ran.  (Measured: heap rw-p before
 * the call, after the call, and after a later malloc.)
 *
 * So the personality has to be set and then handed to a fresh execve.  That is
 * all setarch(8) -X does; doing it in-process just means re-exec'ing ourselves.
 *
 * BITNESS: this only works for 32-bit (-m32) binaries.  set_personality_64bit()
 * clears READ_IMPLIES_EXEC on every execve of a 64-bit ELF, so a 64-bit process
 * can never obtain an executable heap this way -- design 64-bit variants around
 * a route that executes no attacker-supplied bytes instead.
 *
 * NOTE: an executable STACK does not need any of this; -z execstack alone gives
 * [stack] rwxp on both bitnesses.  READ_IMPLIES_EXEC is about the HEAP.
 */
#define REEXEC_GUARD "CHALLENGE_RIE_REEXEC"

static int heap_is_executable(void) {
    FILE *m = fopen("/proc/self/maps", "r");
    if (!m) return 0;
    char line[512];
    int x = 0;
    while (fgets(line, sizeof line, m))
        if (strstr(line, "[heap]")) { x = strstr(line, "rwx") != NULL; break; }
    fclose(m);
    return x;
}

/* Re-exec once with READ_IMPLIES_EXEC so the loader applies it to the new image.
 * Returns only if the personality could not be obtained, or if we already tried. */
static void ensure_exec_heap(char **argv) {
    if (getenv(REEXEC_GUARD)) return;          /* already re-exec'd; do not loop */

    int cur = personality(0xffffffff);         /* query form; never fails on a sane host */
    if (cur == -1) return;
    if (cur & READ_IMPLIES_EXEC) return;       /* setarch -X already did it for us */

    errno = 0;
    if (personality(cur | READ_IMPLIES_EXEC) == -1) {
        /* seccomp can reject the call without setting errno; strerror() then
         * reports "Success", which reads as a bug rather than a denial. */
        int e = errno;
        fprintf(stderr,
            "WARNING: personality(READ_IMPLIES_EXEC) denied (%s).\n"
            "WARNING: the sandbox seccomp profile is blocking the personality syscall,\n"
            "WARNING: so the heap cannot be made executable and the intended solve\n"
            "WARNING: cannot finish.  Serving anyway so the service is not a dead socket.\n",
            e ? strerror(e) : "blocked by sandbox seccomp policy");
        return;                                /* degrade, do not exit */
    }

    setenv(REEXEC_GUARD, "1", 1);
    execv("/proc/self/exe", argv);             /* the execve is what applies it */

    /* only reached if execv failed */
    fprintf(stderr, "WARNING: re-exec failed (%s); continuing without an executable heap.\n",
            strerror(errno));
    personality(cur);                          /* put it back so nothing is half-applied */
    unsetenv(REEXEC_GUARD);
}

int main(int argc, char **argv) {
    (void)argc;
    ensure_exec_heap(argv);

    if (!heap_is_executable())
        fprintf(stderr, "WARNING: heap is not executable; the intended exploit path is closed.\n");

    /* ---- the real challenge service would run here ---- */
    char flag[128] = {0};
    FILE *f = fopen("flag.txt", "r");
    if (f) { fgets(flag, sizeof flag, f); fclose(f); }
    printf("heap executable: %s\nflag: %s\n", heap_is_executable() ? "yes" : "no", flag);
    fflush(stdout);
    return 0;
}
