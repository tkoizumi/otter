# Changelog

Otter's user-visible history. Release notes are curated, one file per version
under `docs/releases/`; the tag publishes that file as the GitHub release body.
This index links to them.

| Version | Date | Headline | Notes |
| --- | --- | --- | --- |
| `v0.5.1` | 2026-10-07 | Ubuntu packages: a `.deb` per Linux architecture, so an install is `apt install ./otter_*.deb` rather than two binaries placed by hand | [docs/releases/v0.5.1.md](docs/releases/v0.5.1.md) |
| `v0.5.0` | 2026-10-07 | Self-serve deploys to Otter Cloud: `otter login`, `otter deploy --cloud`, org-bound credentials, and one-container pooled tenants split across two uids | [docs/releases/v0.5.0.md](docs/releases/v0.5.0.md) |
| `v0.4.0` | 2026-10-03 | The interface freeze: scoped control credential, first-class dynamic schedules, pinned job configuration, a reachable-but-scoped remote API, and the published compatibility policy | [docs/releases/v0.4.0.md](docs/releases/v0.4.0.md) |
| `v0.3.0` | 2026-10-02 | Operable unattended: watch an upgrade, queue age and freshness on `/health`, automatic retention, and the operating procedures as drills | [docs/releases/v0.3.0.md](docs/releases/v0.3.0.md) |
| `v0.2.0` | 2026-09-28 | Dependable execution: recovery is complete, startup can refuse, Linux children die with the daemon, runtime contract published | [docs/releases/v0.2.0.md](docs/releases/v0.2.0.md) |
| `v0.1.19` | 2026-09-28 | Stop losing accepted work: complete recovery reads, re-enqueue claimed runs, release leaked capacity | commit subjects only |
