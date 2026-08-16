#ifndef _POSIX_COMPAT_H
#define _POSIX_COMPAT_H
/*
 * MinGW-w64 defines _ino_t as unsigned short (16-bit) and MSVCRT never
 * fills st_ino at all — which made mkfs treat every file as a hardlink
 * of inode 0 and emit self-referential images (a file dirent aliasing
 * the root directory's nid).  Take over the typedef before any CRT
 * header runs so struct stat can carry a full 64-bit NTFS file index.
 * This is only sound because the stat family below never calls the CRT
 * implementations (their notion of the struct layout still has the
 * 16-bit field): stat/lstat/fstat are macro-redirected to Win32-native
 * implementations at the bottom of this header, and this header is
 * force-included (-include) as the first thing in every translation
 * unit, so no erofs-utils code can reach the CRT versions.
 */
#define _INO_T_DEFINED
typedef unsigned long long _ino_t;
typedef unsigned long long ino_t;

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

static inline int __pc_stat_from_handle(HANDLE h, struct stat* st,
                                        int is_symlink, const char* linkpath) {
  BY_HANDLE_FILE_INFORMATION bi;
  if (!GetFileInformationByHandle(h, &bi)) { errno = EIO; return -1; }
  memset(st, 0, sizeof(*st));
  st->st_dev = (unsigned int)bi.dwVolumeSerialNumber;
  st->st_ino = ((unsigned long long)bi.nFileIndexHigh << 32) | bi.nFileIndexLow;
  st->st_nlink = (short)(bi.nNumberOfLinks ? bi.nNumberOfLinks : 1);
  st->st_mtime = __pc_ft2unix(&bi.ftLastWriteTime);
  st->st_atime = __pc_ft2unix(&bi.ftLastAccessTime);
  st->st_ctime = __pc_ft2unix(&bi.ftCreationTime);
  if (is_symlink) {
    char tgt[4096];
    ssize_t n = __pc_readlink(linkpath, tgt, sizeof(tgt));
    st->st_mode = S_IFLNK | 0777;
    st->st_size = n > 0 ? n : 0;
  } else if (bi.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY) {
    st->st_mode = S_IFDIR | 0755;
  } else {
    st->st_mode = S_IFREG |
        ((bi.dwFileAttributes & FILE_ATTRIBUTE_READONLY) ? 0444 : 0644);
    st->st_size = ((long long)bi.nFileSizeHigh << 32) | bi.nFileSizeLow;
  }
  return 0;
}

static inline int __pc_statx(const char* path, struct stat* st, int follow) {
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
  int r = __pc_stat_from_handle(h, st, is_symlink, path);
  CloseHandle(h);
  return r;
}

static inline int __pc_stat(const char* path, struct stat* st) {
  return __pc_statx(path, st, 1);
}
static inline int __pc_lstat(const char* path, struct stat* st) {
  return __pc_statx(path, st, 0);
}
static inline int __pc_fstat(int fd, struct stat* st) {
  HANDLE h = (HANDLE)_get_osfhandle(fd);
  if (h == INVALID_HANDLE_VALUE) { errno = EBADF; return -1; }
  if (GetFileType(h) != FILE_TYPE_DISK) {
    memset(st, 0, sizeof(*st));
    st->st_mode = S_IFCHR | 0644;
    st->st_nlink = 1;
    return 0;
  }
  return __pc_stat_from_handle(h, st, 0, NULL);
}

/*
 * stat must be a function-like macro: an object-like one would also
 * rewrite the type name in `struct stat`.  The others are object-like
 * on purpose — erofs_vfops has a member named fstat, and an object-like
 * macro renames the member declaration and its uses consistently, while
 * a function-like macro would rewrite only the call sites.
 */
#define stat(path, st)  __pc_stat((path), (st))
#define lstat           __pc_lstat
#define fstat           __pc_fstat
#define readlink        __pc_readlink

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
