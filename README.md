# UpUpUp Monorepo

This repository now houses multiple Go subprojects that collaboratively power the UpUpUp platform.

## Projects

- `worker/` – the original monitoring engine. See `worker/README.md` for full documentation, configuration examples, and Docker usage.
- `server/` – placeholder for the upcoming control plane and API surface.
- `upgent/` – placeholder for generation tooling and auxiliary utilities.

Each project is an independent Go module. Create a personal `go.work` file if you want to develop several modules at once.

## Getting Started

1. Install Go 1.24 (or later).
2. Choose the module you want to work on (for example `cd worker/`) and run Go commands from there.
3. For multi-module workflows, create your own `go.work` alongside the modules you wish to include.

## Repository Layout

```
LICENSE
README.md          # this file
server/            # future server module (placeholder)
upgent/            # future tooling module (placeholder)
worker/            # monitoring engine (formerly repository root)
```

## Licensing

The repository is licensed under the Apache License 2.0. See `LICENSE` for details.



