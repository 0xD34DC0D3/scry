# scry

A small, self-hosted RSS reader in Go: one binary, SQLite, server-rendered
pages. Sign-in is delegated to [voodoo](https://github.com/0xD34DC0D3/voodoo).
It is also the source of the org's Go app template.

Status: early. Admins manage the feed catalog at `/admin/feeds`; feeds are
fetched on a schedule. Anyone can read the latest items at `/`, the catalog
at `/feeds` and one feed at `/feeds/{id}` (excerpts, linking out).
Signed-in users subscribe to feeds and read full items, with unread
tracking, at `/reading`.
The plan is in [docs/initial_milestone.md](docs/initial_milestone.md) and the
spec in [docs/design.md](docs/design.md).

## Run

```sh
cp .env.example .env
make run
curl localhost:8081/healthz
```

`scry doctor` checks the configuration without starting the server.

## License

[MIT](LICENSE), except the third-party files below.

### Third-party files

- `internal/web/static/htmx.min.js`: [htmx](https://htmx.org), 0BSD; the
  version is in `internal/web/web.go`.
- `internal/web/static/fonts/`: Atkinson Hyperlegible Next and JetBrains
  Mono, SIL Open Font License 1.1
  ([OFL.txt](internal/web/static/fonts/OFL.txt)).
