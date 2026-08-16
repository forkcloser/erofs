#ifndef _POSIX_COMPAT_H
#define _POSIX_COMPAT_H
/*
 * This header is force-included (-include) as the first thing in every
 * erofs-utils translation unit built for Windows. It must never change
 * the layout of any CRT type: struct stat in particular is defined by
 * whichever CRT header runs first in a given TU (sys/stat.h, wchar.h and
 * _mingw_stat64.h all reach it), and a typedef that differs between TUs
 * gives them different field offsets for the same struct — st_size
 * written at one offset and read at another comes back as zero. The
 * 64-bit inode identity that mkfs needs and the CRT's 16-bit st_ino
 * cannot hold is carried beside the struct instead (see __pc_lstat_id).
 */

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#ifndef NOMINMAX
#define NOMINMAX
#endif

#include <sys/stat.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <stdio.h>
#include <errno.h>
#include <windows.h>
#include <winioctl.h>
#include <io.h>
/*
 * MinGW-w64 defines uid_t/gid_t as 'short' (signed 16-bit), which
 * truncates values >= 32768 and sign-extends them when promoted to
 * 32-bit.  Override with macros so all subsequent code (including
 * erofs-utils internal structs) sees 32-bit unsigned types, matching
 * the Linux EROFS on-disk format.
 */
#define uid_t unsigned int
#define gid_t unsigned int
/*
 * MinGW-w64 honours _FILE_OFFSET_BITS=64 for off_t (making it 64-bit),
 * but does NOT remap lseek/ftruncate to their 64-bit variants the way
 * glibc does on Linux.  Provide the remapping here so every call site
 * automatically gets 64-bit offsets.
 *
 * These must come AFTER the system headers (included above) so the
 * original prototypes are already declared.
 */
#define lseek  lseek64
#define ftruncate ftruncate64

/*
 * MinGW may define S_IFBLK with a non-Linux value (0x3000 vs 0x6000).
 * erofs uses the Linux on-disk format, so force Linux values here.
 * S_IFLNK and S_IFSOCK are not defined by MinGW at all.
 */
#ifdef S_IFBLK
#undef S_IFBLK
#endif
#define S_IFBLK 0060000
#ifndef S_IFLNK
#define S_IFLNK 0120000
#endif
#ifndef S_IFSOCK
#define S_IFSOCK 0140000
#endif

#ifndef S_ISLNK
#define S_ISLNK(m) (((m) & 0xF000) == 0xA000)
#endif
#ifndef S_ISSOCK
#define S_ISSOCK(m) (((m) & 0xF000) == 0xC000)
#endif
static inline char* strndup(const char* s, size_t n) {
  size_t len = strnlen(s, n);
  char* result = malloc(len + 1);
  if (result) { memcpy(result, s, len); result[len] = 0; }
  return result;
}
static inline char* realpath(const char* path, char* resolved) {
  char* buf = resolved;
  if (!buf) buf = malloc(260);
  if (!buf) return NULL;
  char* result = _fullpath(buf, path, 260);
  if (!result && !resolved) free(buf);
  return result;
}
static inline ssize_t pread(int fd, void* buf, size_t count, off_t offset) {
  off_t old = lseek64(fd, 0, SEEK_CUR);
  if (old < 0) return -1;
  if (lseek64(fd, offset, SEEK_SET) < 0) return -1;
  ssize_t r = read(fd, buf, count);
  lseek64(fd, old, SEEK_SET);
  return r;
}
static inline ssize_t pwrite(int fd, const void* buf, size_t count, off_t offset) {
  off_t old = lseek64(fd, 0, SEEK_CUR);
  if (old < 0) return -1;
  if (lseek64(fd, offset, SEEK_SET) < 0) return -1;
  ssize_t r = write(fd, buf, count);
  lseek64(fd, old, SEEK_SET);
  return r;
}
static inline int fsync(int fd) { return _commit(fd); }

/*
 * Win32-native stat family.
 *
 * The CRT versions are unusable for mkfs: st_ino is never populated
 * (so hardlink dedup collapses the whole tree onto one inode), lstat
 * does not exist (MSVCRT stat follows symlinks), and readlink has no
 * CRT equivalent at all.  Everything below goes straight to Win32:
 * st_ino/st_dev come from the NTFS file index + volume serial (the
 * same identity Windows hardlinks share, so dedup is actually correct),
 * symlinks are detected via the reparse tag, and readlink extracts the
 * target from the reparse data.
 *
 * Mode bits are approximated (0755 dirs, 0644/0444 files, 0777 links):
 * NTFS has no POSIX permissions to preserve.
 */

/* Local mirror of REPARSE_DATA_BUFFER; mingw ships it in ddk/ntifs.h
 * which cannot be included alongside user-mode windows.h. */
typedef struct {
  ULONG  ReparseTag;
  USHORT ReparseDataLength;
  USHORT Reserved;
  union {
    struct {
      USHORT SubstituteNameOffset;
      USHORT SubstituteNameLength;
      USHORT PrintNameOffset;
      USHORT PrintNameLength;
      ULONG  Flags;
      WCHAR  PathBuffer[1];
    } SymbolicLinkReparseBuffer;
    struct {
      USHORT SubstituteNameOffset;
      USHORT SubstituteNameLength;
      USHORT PrintNameOffset;
      USHORT PrintNameLength;
      WCHAR  PathBuffer[1];
    } MountPointReparseBuffer;
  } u;
} __pc_reparse_data;

static inline ssize_t __pc_readlink(const char* path, char* buf, size_t bufsiz) {
  HANDLE h = CreateFileA(path, FILE_READ_ATTRIBUTES,
                         FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                         NULL, OPEN_EXISTING,
                         FILE_FLAG_BACKUP_SEMANTICS | FILE_FLAG_OPEN_REPARSE_POINT,
                         NULL);
  if (h == INVALID_HANDLE_VALUE) { errno = ENOENT; return -1; }
  char raw[MAXIMUM_REPARSE_DATA_BUFFER_SIZE];
  DWORD got = 0;
  BOOL ok = DeviceIoControl(h, FSCTL_GET_REPARSE_POINT, NULL, 0,
                            raw, sizeof(raw), &got, NULL);
  CloseHandle(h);
  if (!ok) { errno = EINVAL; return -1; }
  __pc_reparse_data* rd = (__pc_reparse_data*)raw;
  if (rd->ReparseTag != IO_REPARSE_TAG_SYMLINK) { errno = EINVAL; return -1; }
  const WCHAR* base = rd->u.SymbolicLinkReparseBuffer.PathBuffer;
  const WCHAR* name;
  int wlen;
  if (rd->u.SymbolicLinkReparseBuffer.PrintNameLength > 0) {
    name = base + rd->u.SymbolicLinkReparseBuffer.PrintNameOffset / sizeof(WCHAR);
    wlen = rd->u.SymbolicLinkReparseBuffer.PrintNameLength / sizeof(WCHAR);
  } else {
    name = base + rd->u.SymbolicLinkReparseBuffer.SubstituteNameOffset / sizeof(WCHAR);
    wlen = rd->u.SymbolicLinkReparseBuffer.SubstituteNameLength / sizeof(WCHAR);
    if (wlen >= 4 && wcsncmp(name, L"\\??\\", 4) == 0) { name += 4; wlen -= 4; }
  }
  char tmp[4096];
  int n = WideCharToMultiByte(CP_UTF8, 0, name, wlen, tmp, sizeof(tmp), NULL, NULL);
  if (n <= 0) { errno = EINVAL; return -1; }
  for (int i = 0; i < n; i++)
    if (tmp[i] == '\\') tmp[i] = '/';
  if ((size_t)n > bufsiz) n = (int)bufsiz;
  memcpy(buf, tmp, n);
  return n;
}

static inline time_t __pc_ft2unix(const FILETIME* ft) {
  unsigned long long v =
      ((unsigned long long)ft->dwHighDateTime << 32) | ft->dwLowDateTime;
  if (v < 116444736000000000ULL) return 0;
  return (time_t)((v - 116444736000000000ULL) / 10000000ULL);
}

/*
 * The stat family below is deliberately split in two.
 *
 * mingw-w64 has several struct stat layouts (stat, _stat64, _stat64i32,
 * ...) and `#define stat _stat64`-style redirects that fire depending on
 * _FILE_OFFSET_BITS and on which CRT header happened to run first in a
 * translation unit. Naming `struct stat` in a prototype inside this
 * header pins the prototype to whichever layout was live *here*, while
 * the caller may hold a different one — and then st_size written at one
 * offset is read at another and comes back as zero (that was CI's
 * "dir/b.txt = \"\"" on the mingw-w64 v11 / msvcrt toolchain; the v12+
 * UCRT toolchains agree with themselves and hid it).
 *
 * So the layout-independent core fills a POD of our own (__pc_finfo),
 * and the stat/lstat/fstat entry points are MACROS that copy it into the
 * caller's struct — expanded in the caller's TU, so `->st_size` there is
 * the caller's field at the caller's offset, by construction.
 */
typedef struct {
  unsigned long long ino;   /* NTFS file index: what hard links share */
  unsigned int dev;         /* volume serial */
  unsigned int mode;
  unsigned int nlink;
  long long size;
  long long mtime, atime, ctime;
} __pc_finfo;

static inline int __pc_finfo_from_handle(HANDLE h, __pc_finfo* fi,
                                         int is_symlink, const char* linkpath) {
  BY_HANDLE_FILE_INFORMATION bi;
  if (!GetFileInformationByHandle(h, &bi)) { errno = EIO; return -1; }
  memset(fi, 0, sizeof(*fi));
  fi->ino = ((unsigned long long)bi.nFileIndexHigh << 32) | bi.nFileIndexLow;
  fi->dev = (unsigned int)bi.dwVolumeSerialNumber;
  fi->nlink = bi.nNumberOfLinks ? bi.nNumberOfLinks : 1;
  fi->mtime = __pc_ft2unix(&bi.ftLastWriteTime);
  fi->atime = __pc_ft2unix(&bi.ftLastAccessTime);
  fi->ctime = __pc_ft2unix(&bi.ftCreationTime);
  if (is_symlink) {
    char tgt[4096];
    ssize_t n = __pc_readlink(linkpath, tgt, sizeof(tgt));
    fi->mode = S_IFLNK | 0777;
    fi->size = n > 0 ? n : 0;
  } else if (bi.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY) {
    fi->mode = S_IFDIR | 0755;
  } else {
    fi->mode = S_IFREG |
        ((bi.dwFileAttributes & FILE_ATTRIBUTE_READONLY) ? 0444 : 0644);
    fi->size = ((long long)bi.nFileSizeHigh << 32) | bi.nFileSizeLow;
  }
  return 0;
}

static inline int __pc_finfo_path(const char* path, __pc_finfo* fi, int follow) {
  DWORD attrs = GetFileAttributesA(path);
  if (attrs == INVALID_FILE_ATTRIBUTES) { errno = ENOENT; return -1; }
  int is_symlink = 0;
  DWORD flags = FILE_FLAG_BACKUP_SEMANTICS;
  if (!follow && (attrs & FILE_ATTRIBUTE_REPARSE_POINT)) {
    /* Only a symlink-tagged reparse point is a symlink; any other kind
     * (mount point, OneDrive placeholder, ...) is statted through. */
    char probe[8];
    if (__pc_readlink(path, probe, sizeof(probe)) >= 0 || errno != EINVAL) {
      is_symlink = 1;
      flags |= FILE_FLAG_OPEN_REPARSE_POINT;
    }
  }
  HANDLE h = CreateFileA(path, FILE_READ_ATTRIBUTES,
                         FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                         NULL, OPEN_EXISTING, flags, NULL);
  if (h == INVALID_HANDLE_VALUE) { errno = ENOENT; return -1; }
  int r = __pc_finfo_from_handle(h, fi, is_symlink, path);
  CloseHandle(h);
  return r;
}

static inline int __pc_finfo_fd(int fd, __pc_finfo* fi) {
  HANDLE h = (HANDLE)_get_osfhandle(fd);
  if (h == INVALID_HANDLE_VALUE) { errno = EBADF; return -1; }
  if (GetFileType(h) != FILE_TYPE_DISK) {
    memset(fi, 0, sizeof(*fi));
    fi->mode = S_IFCHR | 0644;
    fi->nlink = 1;
    return 0;
  }
  return __pc_finfo_from_handle(h, fi, 0, NULL);
}

/* Copy a __pc_finfo into the caller's struct stat, whatever its layout.
 * st_ino gets the truncated index (CRT width); the full identity is
 * available through lstat_id / __PC_STAT_ID below. */
#define __PC_FILL_STAT(st, fi) do {                                     \
    memset((st), 0, sizeof(*(st)));                                     \
    (st)->st_dev   = (fi).dev;                                          \
    (st)->st_ino   = (fi).ino;                                          \
    (st)->st_mode  = (fi).mode;                                         \
    (st)->st_nlink = (fi).nlink;                                        \
    (st)->st_size  = (fi).size;                                         \
    (st)->st_mtime = (fi).mtime;                                        \
    (st)->st_atime = (fi).atime;                                        \
    (st)->st_ctime = (fi).ctime;                                        \
  } while (0)

/* Statement-expression form so the entry points can be used as
 * expressions (`if (lstat(p, &st))`) while still expanding at the call
 * site. GNU C is a hard requirement of erofs-utils anyway. */
#define __pc_stat_impl(path, st, follow) ({                             \
    __pc_finfo __fi; int __r = __pc_finfo_path((path), &__fi, (follow)); \
    if (!__r) __PC_FILL_STAT((st), __fi);                               \
    __r; })
#define __pc_stat(path, st)  __pc_stat_impl((path), (st), 1)
#define __pc_lstat(path, st) __pc_stat_impl((path), (st), 0)
/*
 * lstat plus the file's 64-bit identity: the NTFS file index, which all
 * hard links to a file share and which is unique per volume. Together
 * with st_dev (volume serial) this is what mkfs keys hardlink detection
 * on; the CRT's 16-bit st_ino cannot carry it, so it travels separately.
 * The patched erofs_iget_from_local calls this in place of lstat.
 */
#define __pc_lstat_id(path, st, idp) ({                                 \
    __pc_finfo __fi; int __r = __pc_finfo_path((path), &__fi, 0);       \
    if (!__r) { __PC_FILL_STAT((st), __fi); *(idp) = __fi.ino; }        \
    __r; })

/*
 * Wiring the entry points in without disturbing the CRT.
 *
 * Under _FILE_OFFSET_BITS=64, mingw-w64 (v11, msvcrt) does
 *     #define stat  _stat64
 *     #define fstat _fstat64
 * so that BOTH the function name and the type name `struct stat` resolve
 * to the 64-bit-size variant, in every TU alike. A function-like
 * `#define stat(path, st)` of our own would REPLACE that object-like
 * macro: from then on `struct stat st;` in a caller stops being rewritten
 * (a 48-byte struct with a 32-bit st_size), while anything parsed before
 * the replacement — the CRT prototypes, and this header's own code — saw
 * `struct _stat64` (56 bytes, 64-bit st_size). Two layouts for one name
 * is exactly the corruption we are fixing.
 *
 * So the redirect is left alone and the CRT's *target names* are what we
 * take over: _stat64 / _fstat64 (and the plain names for good measure).
 * The CRT prototypes for _stat64/_fstat64 are declared functions; a
 * function-like macro of the same name is legal C and wins at every call
 * site, while `struct _stat64` — the type the CRT macro expands
 * `struct stat` to — is untouched (types are not macro-expanded via
 * function-like macros: `struct _stat64 st;` has no `(` after the name).
 */
static inline int __pc_fstat(int fd, void* st_void) {
  __pc_finfo fi;
  int r = __pc_finfo_fd(fd, &fi);
  if (r) return r;
  struct stat* st = (struct stat*)st_void;   /* = struct _stat64 here and in every TU */
  __PC_FILL_STAT(st, fi);
  return 0;
}
#define _stat64(path, st)  __pc_stat((path), (st))
/* fstat: object-like on purpose. erofs_vfops has a member named fstat;
 * after the CRT's rewrite it is `_fstat64` in both its declaration and
 * every `vf->ops->fstat(...)` use. An object-like macro renames both
 * consistently (to __pc_fstat — a harmless member name); a function-like
 * one would rename only the uses and break the build. */
#define _fstat64           __pc_fstat
#ifndef stat
#define stat(path, st)     __pc_stat((path), (st))
#endif
#ifndef fstat
#define fstat              __pc_fstat
#endif
#define lstat(path, st)    __pc_lstat((path), (st))
#define readlink           __pc_readlink

/*
 * Linux new_encode_dev/new_decode_dev compatible device number encoding.
 * erofs-utils uses this scheme in erofs_new_encode_dev() / erofs_new_decode_dev().
 *   Bits  0-7:  minor bits 0-7
 *   Bits  8-19: major bits 0-11
 *   Bits 20-31: minor bits 8-19
 *
 * These must be macros (not inline functions) because erofs-utils declares
 * local variables named "major"/"minor" that shadow function names.
 */
#define major(dev) (((unsigned int)(dev) >> 8) & 0xfff)
#define minor(dev) (((unsigned int)(dev) & 0xff) | (((unsigned int)(dev) >> 12) & ~0xffu))
#define makedev(maj, min) (((unsigned int)(min) & 0xff) | ((unsigned int)(maj) << 8) | (((unsigned int)(min) & ~0xffu) << 12))
static inline int fchmod(int fd, mode_t mode) { return 0; }
static inline int getpagesize(void) { return 4096; }
static inline unsigned int getuid(void) { return 0; }
static inline unsigned int getgid(void) { return 0; }
#endif
