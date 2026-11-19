# Idempotency example

A tiny payments service that shows the `idempotency` middleware in action.

The `POST /payments` handler is protected by an `Idempotency-Key`. The first
request with a given key runs the handler and its response is stored. Any retry
with the same key (and the same body) replays the stored response — the handler
does **not** run again, so a retried request cannot charge a card twice.

## Run it

```bash
# from this directory
go run .
```

Then, in another shell:

```bash
# First request: the handler runs and charges once.
curl -i -X POST localhost:8080/payments \
  -H 'Idempotency-Key: abc-123' \
  -H 'Content-Type: application/json' \
  -d '{"amount":100}'

# Retry with the same key: the stored response is replayed.
# Look for `Idempotency-Replayed: true` in the response headers;
# the server log shows `[handler] charging (call #1)` only once.
curl -i -X POST localhost:8080/payments \
  -H 'Idempotency-Key: abc-123' \
  -H 'Content-Type: application/json' \
  -d '{"amount":100}'
```

## What to notice

- **Runs once:** the handler logs `charging (call #N)` once per key.
- **Byte-for-byte replay:** the retry returns the same status and body, with
  `Idempotency-Replayed: true`.
- **Different body, same key is rejected:** send the same key with
  `-d '{"amount":200}'` and the middleware returns `ErrFingerprintMismatch`
  instead of running the handler again.
- **5xx is never stored:** if the handler fails, a retry runs it again.

## Production note

This example uses `store.NewMemoryStore()`, which is local to one process. For
multiple instances, use `store.NewRedisStore(...)` so every instance shares the
same idempotency records and locks.