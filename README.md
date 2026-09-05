# Let's Go Out! ✨

A date invitation app, forked from [vantanferny/letsgoout](https://github.com/vantanferny/letsgoout).

## How it works

1. Create a personalized invite with date ideas and time options.
2. Share the invite link to the recipient so they can choose a vibe and time (***fun fact:*** the recipient will be unable to click the "Decline" button 😉).
3. Use the private status link to view their response.

## What's different?

Compared with the [original project](https://github.com/vantanferny/letsgoout), this fork introduces three major changes:

1. Replaced EmailJS and JSONBin with a lightweight, self-hosted Go service backed by SQLite. Each invitation now generates an **invite link** for the recipient and a **private status link** for the sender to check the response.
2. Added custom date ideas with a searchable Unicode emoji picker, allowing senders to personalize invitations beyond the built-in options.
3. Updated the interface to support the new flow, including separate invite and status screens, revised mobile layouts, and a dedicated accepted-invite view.

## Run locally

```sh
docker build -t letsgoout .
docker run --rm -p 8080:8080 \
  -e PUBLIC_BASE_URL=http://localhost:8080 \
  -v letsgoout-data:/data \
  letsgoout
```

Open [http://localhost:8080](http://localhost:8080).

## Deploy with Coolify

Choose the **Docker Compose** build pack and set the Compose file location to
`/docker-compose.yaml`. Set `PUBLIC_BASE_URL` in Coolify to the public origin,
without a trailing slash, then assign the domain to the `letsgoout` service on
container port `8080`.

The Compose definition mounts a named volume at `/data`, where the application
stores its SQLite database. Coolify prefixes the volume name for the resource
and reuses it across deployments. If replacing an existing Dockerfile-based
deployment that already contains invites, copy `/data/letsgoout.db` from that
deployment before switching; attaching the new volume does not migrate the
existing database automatically.

## Configuration

All configuration is provided through environment variables. `PUBLIC_BASE_URL` is required; the remaining options have production-friendly defaults.

| Variable | Default | Description |
| --- | --- | --- |
| `LISTEN_ADDRESS` | `0.0.0.0:8080` | Address and port served by the application. |
| `PUBLIC_BASE_URL` | *(required)* | Public origin used for exact-origin checks and generated links. Do not include a trailing slash. |
| `DATABASE_PATH` | `/data/letsgoout.db` | SQLite database path. Mount its directory on persistent storage. |
| `GLOBAL_DAILY_LIMIT` | `500` | Successful invite creations allowed during any rolling 24-hour window. |
| `MAX_DATABASE_BYTES` | `268435456` | Maximum SQLite main-file size (256 MiB). The application will not start if an existing database is already larger. |
| `MAX_JOURNAL_BYTES` | `8388608` | Maximum retained SQLite journal/WAL size after a journal reset (8 MiB). |

Limits must be positive integers. The previous IP-based rate-limit variables are no longer used.

## Storage limits and retention

Pending invites expire after seven days. Accepting an invite extends its original expiry by another seven days, so no invite remains active for more than fourteen days. Expired invites and creation events outside the rolling 24-hour window are removed at startup and hourly.

At the default creation limit, the database can contain at most roughly 7,000 active invites. SQLite reuses pages released by deleted rows, so the database file may remain at its previous high-water size instead of shrinking after cleanup. Routine `VACUUM` is intentionally not performed. `MAX_DATABASE_BYTES` provides the hard main-file ceiling; once reached, new invite creation returns a storage-capacity error instead of growing the file further.
