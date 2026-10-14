# feedbench: change-feed cost on writes

Run 2026-10-14 14:00 UTC.

- Postgres 17.10, wal_level=logical, on the same machine as the driver
- Go go1.26.7-X:nodwarf5, linux/amd64, 22 logical CPUs
- 16 writers, 20s measured per mode; each transaction updates one random row of 10000 (1800-byte text column) by primary key

| Mode | What runs | tx/s | vs plain | p50 | p99 | decoded |
|---|---|---|---|---|---|---|
| plain | no feed | 1872 | 100% | 7.83 ms | 41.17 ms | – |
| triggers | InstallTriggers, one listener | 261 | 14% | 61.34 ms | 77.64 ms | – |
| triggers+fence | InstallTriggers and a NOTIFY fence per transaction | 255 | 14% | 59.19 ms | 113.94 ms | – |
| logical | publication (whole rows) and a slot read every 50 ms | 1792 | 96% | 8.23 ms | 16.13 ms | 63.7 MiB |
| logical+fence | logical, and a transactional logical message per transaction | 1588 | 85% | 8.83 ms | 42.53 ms | 57.6 MiB |
