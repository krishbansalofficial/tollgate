# ADR 0002: Store virtual keys as SHA-256, not bcrypt/argon2

Status: accepted (2026-11)

## Context

Raw API keys must never be stored. Password hashing (bcrypt, argon2) is deliberately slow because passwords are low-entropy and guessable.

## Decision

Keys are `tg_` + 256 random bits (base62). Store `sha256(key)` with a unique index and look up by hash. Keep an 8-character display prefix in plain text for support.

## Consequences

A 256-bit random secret cannot be brute-forced, so a fast hash is equally safe, and auth costs microseconds instead of the ~50–100 ms a slow hash adds to every cache-missed request. The comparison happens inside the index lookup, so there is no timing side channel. A leaked database still yields no usable keys.
