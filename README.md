# Let's Go Out! ✨

A date invitation app, forked from [vantanferny/letsgoout](https://github.com/vantanferny/letsgoout).

## How it works

1. Create a personalized invite with date ideas and time options.
2. Share the invite link to the recipient so they can choose a vibe and time (***fun fact:*** the recipient will be unable to click the "Decline" button 😉).
3. Use the private status link to view their response.

## What's different?

Compared with the [original project](https://github.com/vantanferny/letsgoout), this fork introduces three major changes:

1. Replaced EmailJS and JSONBin with a lightweight, self-hosted Go service backed by SQLite. Each invitation now generates an **invite link** for the recipient and a **private status link** for the sender to check the response.
2. Added custom date ideas, allowing senders to personalize invitations beyond the built-in options.
3. Updated the interface to support the new flow, including separate invite and status screens, revised mobile layouts, and a dedicated accepted-invite view.

## Run locally

```sh
docker build -t letsgoout .
docker run --rm -p 8080:8080 \
  -e PUBLIC_BASE_URL=http://localhost:8080 \
  -e RATE_LIMIT_HMAC_KEY=replace-with-at-least-32-random-bytes \
  -e DISABLE_RATE_LIMITS=true \
  -v letsgoout-data:/data \
  letsgoout
```

Open [http://localhost:8080](http://localhost:8080). `DISABLE_RATE_LIMITS=true` is for local development only; omit it in public environments.
