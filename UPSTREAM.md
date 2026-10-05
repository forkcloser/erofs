# Upstream

Fork of <https://github.com/erofs/go-erofs> (`main`).

- Forked from: [`44d5e74`](https://github.com/erofs/go-erofs/commit/44d5e74d4dfbc6c23985fa805cec66cc57fb8558), committed 2026-08-02
- Reviewed through: [`03d68d8`](https://github.com/erofs/go-erofs/commit/03d68d88381cdf8133a0adcc248468d33aa457f5), reviewed 2026-10-05

Every upstream commit after the fork point, through the reviewed one, is in
exactly one of the tables below. A port is a commit of this repository,
adapted to the fork; nothing is merged from upstream.

## Incorporated

| Upstream commit | Here | What |
| --- | --- | --- |
| [`03d68d8`](https://github.com/erofs/go-erofs/commit/03d68d88381cdf8133a0adcc248468d33aa457f5) | [`37b9c5c`](https://github.com/forkcloser/erofs/commit/37b9c5cfad78d7a05d426600a63b902679c1bcac) | `system.posix_acl_*` xattr prefixes lose their trailing dot |

## Not incorporated

| Upstream commit | Why |
| --- | --- |
| [`ba68dad`](https://github.com/erofs/go-erofs/commit/ba68dadcd923ccaae360f13594e62959cd267911) | hardlink support exists here independently ([`a664c24`](https://github.com/forkcloser/erofs/commit/a664c24ed1eec58bbcb2865896e9262f2941d343)) |
| [`112653e`](https://github.com/erofs/go-erofs/commit/112653e5bb40eb59db4ec3c3f08fb9d68a895550) | `Remove` exists here independently ([`1dcedcd`](https://github.com/forkcloser/erofs/commit/1dcedcdfcb27bfa91b3c2be9336c7cc5e3a87080)) |
| [`52cc42c`](https://github.com/erofs/go-erofs/commit/52cc42c5291c08ce1b29d1e78088d12cea4c00df) | `RemoveAll` exists here independently ([`1dcedcd`](https://github.com/forkcloser/erofs/commit/1dcedcdfcb27bfa91b3c2be9336c7cc5e3a87080)) |
