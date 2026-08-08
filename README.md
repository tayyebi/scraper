# Browser Fleet Controller

Turns **real, logged-in browsers** into a programmable endpoint that scrapers and
automations can drive over an API.

Unlike a headless-browser service, the browser is *yours*: real profile, real
cookies, real TLS fingerprint, real session state. That is the whole point — it
can reach pages that require a human to have logged in.

The binary is `hubd`; a deployment is "the hub". It has three planes:

- **Agent Plane** — browsers dial in and hold a command channel open.
- **Control Plane** — the northbound REST API that automations drive.
- **Operator Console** — a web UI that is itself just a Control API client.

See [GLOSSARY.md](GLOSSARY.md) for the vocabulary, [ARCHITECTURE.md](ARCHITECTURE.md)
for the shape, [docs/protocol.md](docs/protocol.md) for the wire format,
[docs/api.md](docs/api.md) for the API, and [docs/quickstart.md](docs/quickstart.md)
to run it end to end.

## Status

Built in the open, one plan step per commit. See [PLAN.md](PLAN.md).

## Building

Nothing is built on the maintainer's machine — there is no Go toolchain there by
design. **CI is the compiler.** Every push to `main` runs
[.github/workflows/ci.yml](.github/workflows/ci.yml), which normalizes
`go.mod`/`go.sum`/`gofmt`, then vets, builds, and tests, emitting every
diagnostic as a check-run annotation so failures are readable over the anonymous
GitHub REST API.

If you *do* have Go 1.23+:

```sh
go build -o hubd ./cmd/hubd
./hubd -addr :8080 -data ./data
```

Extension zips are produced by `go run ./cmd/pack` — no Node, no `npm install`,
no `node_modules`.

## License

See [LICENSE](LICENSE).
