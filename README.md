# Brawl Stars Playbook

An unofficial Brawl Stars companion app. One data and intelligence system powering five decision-making surfaces: Ranked draft assistant, trophy push advisor, personal coaching, meta engine, and patch intelligence.

This is not a stats dashboard. It answers questions: what should I pick, what should I ban, which brawlers should I push, where am I weak?

> This project is not affiliated with, endorsed by, or associated with Supercell. Brawl Stars is a trademark of Supercell Oy.

---

## Architecture

```
Official Brawl Stars API (25 battles per player, IP-restricted key)
              |
         collector binary (Go)
              |
         PostgreSQL 16
              |
    analytics jobs (Python, future)
              |
         api binary (Go, future)
              |
    React + TypeScript frontend (future)
```

Single Go module at the root. Two binaries: `collector` and `api`. Shared packages in `internal/`.

## Quick Start (Local Development)

### Prerequisites

- Go 1.23+
- Docker with Docker Compose
- A Brawl Stars API key from [developer.brawlstars.com](https://developer.brawlstars.com) (IP-restricted: register your current public IP when creating the key)

### Setup

```bash
# 1. Copy and fill in environment variables
cp .env.example .env
# Edit .env and set BRAWLSTARS_API_TOKEN

# 2. Start local PostgreSQL
make dev

# 3. Apply database migrations
make migrate-up

# 4. Seed the patch record (run once)
make psql
# In psql: \i db/seeds/patches.sql

# 5. Seed the brawlers reference table
make seed-brawlers

# 6. Collect a player's data
make collect tag=#YOURTAG
```

### Useful Commands

```bash
make test          # run unit tests (no API or DB required)
make build         # compile both binaries to bin/
make migrate-up    # apply pending migrations
make migrate-down  # roll back last migration
make psql          # open psql shell
make lint          # run go vet
```

## Project Layout

```
internal/
  apiclient/        Typed Brawl Stars API client
  domain/           Core types (TrophyBucket)
  ingestion/        Battle fingerprinting, battle and player ingestors
  storage/          pgxpool connection and typed query functions
collector/cmd/      CLI binary: collect, seed, migrate
api/cmd/            REST API binary (future)
db/migrations/      golang-migrate SQL migration files
db/seeds/           Seed data for local development
deploy/             docker-compose.yml for local dev
docs/               Architecture and API capability documentation
```

## Key Design Decisions

- **Battle deduplication**: The same 3v3 battle appears in up to 6 players' logs. A SHA-256 fingerprint of `(battleTime, eventID, sorted participant tags)` is computed before each insert. `ON CONFLICT DO NOTHING` ensures exactly one row per real-world battle.

- **PostgreSQL as crawl queue**: `crawl_targets` with `FOR UPDATE SKIP LOCKED` gives concurrent-safe worker claiming without Redis.

- **Stratified sampling**: `trophy_bucket_at_discovery` on `crawl_targets` tracks which skill population each player was discovered in. Analytics never present high-trophy data as universal truth.

- **Patch-aware schema**: Every battle and player snapshot has a `patch_id` FK. Analytics windows can be scoped to a single balance state.

- **Data honesty**: Every statistic is labeled DIRECT, DERIVED, MODELED, or UNAVAILABLE. Confidence intervals and sample sizes are exposed wherever shown.

## API Constraints

The official Brawl Stars API has hard limits that shape the entire architecture:

- 25 battles maximum per player (no workaround)
- No ban history available anywhere
- No equipped gadget/star power/gear per battle (cannot do build analytics)
- No Ranked tier/league (trophies are the only skill proxy)
- API keys are IP-whitelisted
- No kill/death/damage statistics

See `docs/data-capabilities.md` for the full capability matrix.

## Milestones

- **M1 (current)**: Player profile + battle log ingestion with deduplication
- **M2**: Continuous crawler with rate limiting and queue scheduling
- **M3**: Analytics aggregation jobs and meta statistics
- **M4**: REST API serving meta and draft data
- **M5**: React frontend with Ranked draft assistant
