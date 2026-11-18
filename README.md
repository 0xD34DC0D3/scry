# scry

A small, self-hosted RSS reader in Go: one binary, SQLite, server-rendered
pages. Sign-in is delegated to [voodoo](https://github.com/0xD34DC0D3/voodoo).
It is also the source of the org's Go app template.

Status: early. Admins manage the feed catalog at `/admin/feeds`; feeds are
fetched on a schedule. Anyone can read the latest items at `/`, the catalog
at `/feeds` and one feed at `/feeds/{id}` (excerpts, linking out).
Signed-in reading and subscriptions are not built yet.
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

[MIT](LICENSE)
