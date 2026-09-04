# Let's Go Out! ✨

A date invitation app, forked from [vantanferny/letsgoout](https://github.com/vantanferny/letsgoout).

## How it works

1. Create a personalized invite with date ideas and time options.
2. Send the invite link so the recipient can choose a vibe and time, and optionally add a message for the sender (***fun fact:*** the recipient will be unable to click the "Decline" button 😉).
3. Keep the private status link to view the response and optional recipient message, or permanently delete the invite.

## What changed

1. Replaced JSONBin and EmailJS with a self-hosted Go and SQLite backend.
2. Added secure invite and status links with acceptance tracking, expiry, and deletion.
3. Bundled the web app into a production-ready, non-root Docker container with security and rate limiting.

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
