# proto/ — Overlord wire schema

This directory is the **source of truth** for every wire-visible type and RPC in
Overlord from Phase 1.1 onwards. The Phase 0 REST + WebSocket contracts under
[`../api/`](../api) are being retired and should not be extended.

## Layout

```
proto/
  overlord/
    v1/
      events.proto     # Envelope + payload oneof (one arm per event type)
      overseer.proto   # OverseerService (Castle <- Overseer)
      castle.proto     # CastleService  (Castle <- Parapet / tooling)
```

Generated code is **checked in** so day-to-day builds do not require `buf`:

- Go:  [`../shared/pb/overlord/v1/`](../shared/pb/overlord/v1)
- TS:  [`../parapet/src/gen/overlord/v1/`](../parapet/src/gen/overlord/v1)

## Editing the schema

1. Edit the relevant `.proto` file(s) under `overlord/v1/`.
2. Keep existing field numbers stable; never reuse numbers.
3. Run `make proto-lint` followed by `make proto` from the repo root.
4. Commit the `.proto` change together with the regenerated files under
   `shared/pb/` and `parapet/src/gen/`.
5. CI enforces both `buf lint` and a drift check (`make proto-check-drift`).

## Tooling

Install [`buf`](https://buf.build/docs/installation) locally:

```bash
brew install bufbuild/buf/buf            # macOS
# or
go install github.com/bufbuild/buf/cmd/buf@latest
```

Plugin versions are pinned in [`../buf.gen.yaml`](../buf.gen.yaml); bump the
Go and TS plugin lines together.
