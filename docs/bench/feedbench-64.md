# feedbench: change-feed cost on writes

Run 2026-10-18 15:01 UTC.

- Postgres 17.10, wal_level=logical, on the same machine as the driver
- Go go1.26.7-X:nodwarf5, linux/amd64, 22 logical CPUs
- 64 writers, 20s measured per mode; each transaction updates one random row of 10000 (100-byte text column) by primary key
- 5 rounds, the modes alternating within each; tx/s, p50 and p99 are each mode's median over the rounds, and "vs plain" compares medians

| Mode | What runs | tx/s | range | vs plain | p50 | p99 |
|---|---|---|---|---|---|---|
| plain | no feed | 6935 | 5970–9444 | 100% | 8.98 ms | 13.29 ms |
| triggers | InstallTriggers, one listener | 347 | 293–481 | 5% | 112.18 ms | 1259.42 ms |
| triggers+fence | InstallTriggers and a NOTIFY fence per transaction | 547 | 225–642 | 8% | 98.61 ms | 262.25 ms |
| logical | InstallPublication (whole rows), an App on FeedLogical streaming its slot | 6006 | 5465–7329 | 87% | 9.49 ms | 68.79 ms |
| logical+fence | logical, and a transactional logical message per transaction | 5439 | 5227–6220 | 78% | 10.43 ms | 42.14 ms |
