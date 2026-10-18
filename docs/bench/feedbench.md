# feedbench: change-feed cost on writes

Run 2026-10-18 14:52 UTC.

- Postgres 17.10, wal_level=logical, on the same machine as the driver
- Go go1.26.7-X:nodwarf5, linux/amd64, 22 logical CPUs
- 16 writers, 20s measured per mode; each transaction updates one random row of 10000 (100-byte text column) by primary key
- 5 rounds, the modes alternating within each; tx/s, p50 and p99 are each mode's median over the rounds, and "vs plain" compares medians

| Mode | What runs | tx/s | range | vs plain | p50 | p99 |
|---|---|---|---|---|---|---|
| plain | no feed | 1991 | 1817–4883 | 100% | 7.59 ms | 40.37 ms |
| triggers | InstallTriggers, one listener | 243 | 216–639 | 12% | 60.12 ms | 263.89 ms |
| triggers+fence | InstallTriggers and a NOTIFY fence per transaction | 327 | 270–640 | 16% | 56.38 ms | 72.40 ms |
| logical | InstallPublication (whole rows), an App on FeedLogical streaming its slot | 1889 | 1811–4452 | 95% | 7.65 ms | 41.12 ms |
| logical+fence | logical, and a transactional logical message per transaction | 2032 | 1756–4372 | 102% | 7.54 ms | 40.75 ms |
