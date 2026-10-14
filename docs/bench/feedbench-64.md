# feedbench: change-feed cost on writes

Run 2026-10-14 13:59 UTC.

- Postgres 17.10, wal_level=logical, on the same machine as the driver
- Go go1.26.7-X:nodwarf5, linux/amd64, 22 logical CPUs
- 64 writers, 20s measured per mode; each transaction updates one random row of 10000 (100-byte text column) by primary key

| Mode | What runs | tx/s | vs plain | p50 | p99 | decoded |
|---|---|---|---|---|---|---|
| plain | no feed | 6594 | 100% | 9.46 ms | 16.99 ms | – |
| triggers | InstallTriggers, one listener | 599 | 9% | 104.87 ms | 132.00 ms | – |
| triggers+fence | InstallTriggers and a NOTIFY fence per transaction | 375 | 6% | 114.61 ms | 328.66 ms | – |
| logical | publication (whole rows) and a slot read every 50 ms | 5904 | 90% | 10.71 ms | 18.29 ms | 19.3 MiB |
| logical+fence | logical, and a transactional logical message per transaction | 4569 | 69% | 10.99 ms | 29.53 ms | 18.0 MiB |
