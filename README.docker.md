# jdwillmsen/minecraft-server-agent

Minecraft Bedrock server chat agent: tool-calling LLM assistant, welcomes, stats.

![Docker Image Version](https://img.shields.io/docker/v/jdwillmsen/minecraft-server-agent?sort=semver)
![Docker Image Size](https://img.shields.io/docker/image-size/jdwillmsen/minecraft-server-agent?sort=semver)
![Docker Pulls](https://img.shields.io/docker/pulls/jdwillmsen/minecraft-server-agent)

## What it is

A Go service that joins a Minecraft Bedrock server as a headless client, reads
chat, and answers `!` commands and `@server` mentions: welcomes, player stats,
a curated knowledge base, waypoints, and a tool-calling LLM assistant. It never
writes to the server console itself; that goes through `mc-console-bridge`.
Postgres and the LLM are both optional.

## Pull

The same image, with the same digest, is published to both registries:

```sh
docker pull ghcr.io/jdwillmsen/minecraft-server-agent:<version>
docker pull docker.io/jdwillmsen/minecraft-server-agent:<version>
```

## Tags

| Tag         | Meaning                                       |
| ----------- | --------------------------------------------- |
| `<version>` | one release (e.g. `0.1.0`), never overwritten |

There is no `latest` tag. Pin an exact version so an upgrade is always a
deliberate change. Images are `linux/amd64` only and carry SLSA provenance and
an SBOM.

## Configuration and docs

Environment variables, the plugin list, the database schema and the release
process are documented in the GitHub README:
<https://github.com/jdwillmsen/minecraft-server-agent#readme>

## Source and license

Source: <https://github.com/jdwillmsen/minecraft-server-agent>
License: PolyForm Noncommercial 1.0.0
