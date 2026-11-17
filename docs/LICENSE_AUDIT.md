# Dependency license audit

Milestone 1, 2026-11-17. Policy: Apache-2.0, MIT, BSD, ISC and MPL-2.0 are permitted. GPL,
AGPL, LGPL, SSPL, BSL and unlicensed code are not.

## Method

A throwaway module imported every package Delta-V plans to link through milestone 8, at the
pinned versions. `scripts/licenses.sh` ran [go-licenses](https://github.com/google/go-licenses)
v2.0.1 (Apache-2.0; a dev tool, never linked) over its transitive package graph for
linux/amd64, linux/arm64 and darwin/arm64, then took the union. Linux adds
`prometheus/procfs`. The same script gates this repo: `make licenses` fails the build on any
license outside the list, and `make license-report` regenerates `THIRD_PARTY_LICENSES.md`
and `third_party/licenses/` from what is actually linked.

**Result: 41 libraries, all permitted.** 18 MIT, 14 Apache-2.0, 9 BSD-3-Clause, no unknowns.
23 are linked today and listed in `THIRD_PARTY_LICENSES.md`. These 18 come in with the TUI
(milestone 6):

| Library | Version | License | Copyright |
|---|---|---|---|
| charm.land/bubbles/v2 | v2.2.1 | MIT | Copyright (c) 2020-2026 Charmbracelet, Inc. |
| charm.land/bubbletea/v2 | v2.1.1 | MIT | Copyright (c) 2020-2026 Charmbracelet, Inc. |
| charm.land/lipgloss/v2 | v2.1.1 | MIT | Copyright (c) 2021-2026 Charmbracelet, Inc. |
| github.com/atotto/clipboard | v0.1.4 | BSD-3-Clause | Copyright (c) 2013 Ato Araki. All rights reserved. |
| github.com/charmbracelet/colorprofile | v0.4.3 | MIT | Copyright (c) 2020-2024 Charmbracelet, Inc |
| github.com/charmbracelet/ultraviolet | v0.0.0-20261008173134-6b8d4baf91b4 | MIT | Copyright (c) 2025 Charmbracelet, Inc |
| github.com/charmbracelet/x/ansi | v0.11.9 | MIT | Copyright (c) 2023 Charmbracelet, Inc. |
| github.com/charmbracelet/x/term | v0.2.2 | MIT | Copyright (c) 2023 Charmbracelet, Inc. |
| github.com/charmbracelet/x/termios | v0.1.1 | MIT | Copyright (c) 2023 Charmbracelet, Inc. |
| github.com/charmbracelet/x/windows | v0.2.2 | MIT | Copyright (c) 2023 Charmbracelet, Inc. |
| github.com/clipperhouse/displaywidth | v0.11.0 | MIT | Copyright (c) 2025 Matt Sherman |
| github.com/clipperhouse/uax29/v2/graphemes | v2.7.0 | MIT | Copyright (c) 2020 Matt Sherman |
| github.com/lucasb-eyer/go-colorful | v1.4.1 | MIT | Copyright (c) 2013 Lucas Beyer |
| github.com/mattn/go-runewidth | v0.0.30 | MIT | Copyright (c) 2016 Yasuhiro Matsumoto |
| github.com/muesli/cancelreader | v0.2.2 | MIT | Copyright (c) 2022 Erik Geiser and Christian Muehlhaeuser |
| github.com/rivo/uniseg | v0.4.7 | MIT | Copyright (c) 2019 Oliver Kuederle |
| github.com/xo/terminfo | v1.0.0 | MIT | Copyright (c) 2016 Anmol Sethi |
| golang.org/x/sync/errgroup | v0.23.0 | BSD-3-Clause | Copyright 2009 The Go Authors. |

## Non-Go components

| Component | Version | License | Use | Obligations |
|---|---|---|---|---|
| htmx and its `hx-sse` extension | 4.0.1 | 0BSD | Vendored into the embedded UI (milestone 5) | None. 0BSD is the zero-clause BSD, more permissive than BSD-2; approved as "BSD" on 2026-11-17. |
| Tailwind CSS standalone CLI | 4.3.5 | MIT, © Tailwind Labs, Inc. | Build tool run by the Makefile; not shipped | The generated CSS embeds Tailwind's preflight. Keep the `/*! tailwindcss … MIT License */` banner in the committed CSS. |
| Envoy | 1.40.2 | Apache-2.0 | Unmodified upstream binary on client hosts and in the test harness; not linked | If we ever *ship* Envoy binaries, include its LICENSE and NOTICE. |

## Notes

- Five Apache-2.0 libraries made of generated protobuf code (`cel.dev/expr`, `cncf/xds/go`,
  go-control-plane `envoy` and `ratelimit`, `protoc-gen-validate`) name no copyright holder in
  their LICENSE, NOTICE or source headers. `THIRD_PARTY_LICENSES.md` says so rather than
  guessing.
- Five Apache-2.0 libraries ship a NOTICE file (gRPC and four Prometheus modules). Their NOTICE
  files are copied with their LICENSEs, as Apache-2.0 §4(d) requires for redistribution.
- No code has been copied or adapted from any upstream project, including MinIO sidekick and
  service mesh control planes.
- Delta-V itself is MIT-licensed (`LICENSE`), decided 2026-11-17.
