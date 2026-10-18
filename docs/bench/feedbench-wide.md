# feedbench: change-feed cost on writes

Run 2026-10-18 15:11 UTC.

- Postgres 17.10, wal_level=logical, on the same machine as the driver
- Go go1.26.7-X:nodwarf5, linux/amd64, 22 logical CPUs
- 16 writers, 20s measured per mode; each transaction updates one random row of 10000 (1800-byte text column) by primary key
- 5 rounds, the modes alternating within each; tx/s, p50 and p99 are each mode's median over the rounds, and "vs plain" compares medians

| Mode | What runs | tx/s | range | vs plain | p50 | p99 |
|---|---|---|---|---|---|---|
| plain | no feed | 1955 | 1509–2399 | 100% | 7.69 ms | 41.07 ms |
| triggers | InstallTriggers, one listener | 228 | 176–239 | 12% | 62.04 ms | 341.44 ms |
| triggers+fence | InstallTriggers and a NOTIFY fence per transaction | 211 | 196–290 | 11% | 61.31 ms | 387.97 ms |
| logical | InstallPublication (whole rows), an App on FeedLogical streaming its slot | 1647 | 1451–1890 | 84% | 8.13 ms | 51.37 ms |
| logical+fence | logical, and a transactional logical message per transaction | 1708 | 1452–1885 | 87% | 8.00 ms | 61.68 ms |
