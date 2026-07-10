# Contributing

Contributions are welcome — bug reports, fixes, features, and docs alike.

## Ground rules

- **Build/test before you push**: `go build ./... && go vet ./... && go test ./...`
  must be clean, and `gofmt -w` run on touched files. CI enforces all three.
- **Wire-format changes need executable proof.** Anything that composes or
  parses Minecraft packets ships with a byte-oracle or structural re-parse
  test — this rule has caught real client-breaking bugs and is non-negotiable.
- **The engine speaks domain events, never wire.** New client-visible features
  enter as a typed attach frame + a renderer in `tachyne-common` + gateway
  wiring + engine emission. PRs that put Minecraft byte composition in the
  world engine will be asked to restructure.
- **Vanilla behavior facts only.** Game constants must come from observable
  behavior, published data (minecraft-data, mcmeta, the Minecraft Wiki), or
  Mojang's officially published mappings — never copied code. GPL projects
  (e.g. ViaVersion) may be cited as factual references only.

## Licensing of contributions

The project is Apache-2.0. Per its section 5, any contribution you
intentionally submit is licensed under the same terms, with no separate CLA.
Please make sure you have the right to submit what you contribute.

## Getting oriented

Each repo's README explains its role; `tachyne-world/docs/` holds the deep
design docs (domain events, sharding, earth mode, vanilla-parity notes).
