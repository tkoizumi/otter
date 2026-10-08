# Changelog

Otter's user-visible history. Release notes are curated, one file per version
under `docs/releases/`; the tag publishes that file as the GitHub release body.
This index links to them.

| Version | Date | Headline | Notes |
| --- | --- | --- | --- |
| `v0.5.4` | 2026-10-08 | Per-job desired state: one entry per job under the runtime generation fence, so a delete sticks and one job's deploy no longer disturbs another; Cloud reports a removal only once the runtime confirmed it | [docs/releases/v0.5.4.md](docs/releases/v0.5.4.md) |
| `v0.5.3` | 2026-10-08 | Delete a job from the CLI: `otter delete --cloud <name|id>` behind an irreversible-action confirmation that names the job and refuses a directory | [docs/releases/v0.5.3.md](docs/releases/v0.5.3.md) |
| `v0.5.2` | 2026-10-07 | Deploy what you released: `otter deploy --cloud` promotes an existing release (`--build` restores packaging), and jobs can be deleted | [docs/releases/v0.5.2.md](docs/releases/v0.5.2.md) |
| `v0.5.1` | 2026-10-07 | Ubuntu packages: a `.deb` per Linux architecture, so an install is `apt install ./otter_*.deb` rather than two binaries placed by hand | [docs/releases/v0.5.1.md](docs/releases/v0.5.1.md) |
| `v0.5.0` | 2026-10-07 | Self-serve deploys to Otter Cloud: `otter login`, `otter deploy --cloud`, org-bound credentials, and one-container pooled tenants split across two uids | [docs/releases/v0.5.0.md](docs/releases/v0.5.0.md) |
| `v0.4.0` | 2026-10-03 | The interface freeze: scoped control credential, first-class dynamic schedules, pinned job configuration, a reachable-but-scoped remote API, and the published compatibility policy | [docs/releases/v0.4.0.md](docs/releases/v0.4.0.md) |
| `v0.3.0` | 2026-10-02 | Operable unattended: watch an upgrade, queue age and freshness on `/health`, automatic retention, and the operating procedures as drills | [docs/releases/v0.3.0.md](docs/releases/v0.3.0.md) |
| `v0.2.0` | 2026-09-28 | Dependable execution: recovery is complete, startup can refuse, Linux children die with the daemon, runtime contract published | [docs/releases/v0.2.0.md](docs/releases/v0.2.0.md) |
| `v0.1.19` | 2026-09-28 | Stop losing accepted work: complete recovery reads, re-enqueue claimed runs, release leaked capacity | commit subjects only |
