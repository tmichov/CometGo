# feedbench: change-feed cost on writes

Run 2026-10-14 13:57 UTC.

- Postgres 17.10, wal_level=logical, on the same machine as the driver
- Go go1.26.7-X:nodwarf5, linux/amd64, 22 logical CPUs
- 16 writers, 20s measured per mode; each transaction updates one random row of 10000 (100-byte text column) by primary key

| Mode | What runs | tx/s | vs plain | p50 | p99 | decoded |
|---|---|---|---|---|---|---|
| plain | no feed | 5031 | 100% | 3.10 ms | 5.01 ms | – |
| triggers | InstallTriggers, one listener | 624 | 12% | 24.82 ms | 36.53 ms | – |
| triggers+fence | InstallTriggers and a NOTIFY fence per transaction | 654 | 13% | 23.92 ms | 35.51 ms | – |
| logical | publication (whole rows) and a slot read every 50 ms | 3600 | 72% | 4.30 ms | 7.42 ms | 12.1 MiB |
| logical+fence | logical, and a transactional logical message per transaction | 3728 | 74% | 4.00 ms | 8.16 ms | 14.7 MiB |
