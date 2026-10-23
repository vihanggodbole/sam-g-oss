# sam-g

A small Go runtime for persistent agent work. Embed it in an application, or
control it through a thin HTTP/SSE server. Conversations, tool outcomes and events
are saved in SQLite; disconnecting a client does not cancel accepted work.

## Try it locally

Requires **Go 1.27+** on Linux or macOS. From this checkout:

```sh
go run ./example
```

Open **http://127.0.0.1:8765**. The example serves its own page and backend, supports
named ChatGPT/Copilot accounts, and keeps private data under `~/.sam-g/example/`.
Tools are off by default. Provider eligibility and model access depend on your
account; sending messages can consume its allowance. See the
[example walkthrough](example/README.md) before enabling file or shell tools.

For API-key configuration, standalone server installation, or Go embedding, use
the [main guide](docs/README.md).

## Safety boundary

**This is a trusted-user runtime, not a sandbox or public multi-user service.**
Enabled tools run with the host OS user's permissions, including access outside
the selected workspace. The stock API has no user authentication; keep it behind
loopback, SSH or restricted Tailscale access. See
[trust and recovery](docs/README.md#trust-boundary-and-recovery).

## Documentation

- [Overview and quick start](docs/README.md)
- [Go library](docs/LIBRARY.md) and [HTTP API](docs/SERVER.md)
- [Authentication and discovery](docs/AUTH-MODELS.md)
- [Model installation and handoffs](docs/MODEL-INSTALLATION.md)
- [Remote deployment](docs/SERVER.md#deployment-on-a-remote-machine)
- [Current limitations](docs/README.md#current-limitations)
- [Contributor navigation and tests](docs/maintainers/README.md)

The Go module is `github.com/vihanggodbole/sam-g-oss`. Until a release is published,
use the local `replace` setup in the [embedding guide](docs/README.md#embed-the-go-core).
Released under the [MIT License](LICENSE).
