Files that are not Go modules and are shipped inside the `deltav` binary as part of the web UI
(`internal/webui/static/`). Their licenses are kept under `third_party/static/`.

| Component | Version | License | Copyright (from upstream files) | Shipped as |
|---|---|---|---|---|
| [htmx](https://htmx.org) and its SSE extension | 4.0.1 | 0BSD | not stated in upstream files | `htmx.min.js` and `hx-sse.min.js`: `dist/htmx.min.js` and `dist/ext/hx-sse.min.js` of the npm package `htmx.org@4.0.1`, unmodified. A test pins the SHA-256 of both. |
| [Tailwind CSS](https://tailwindcss.com) | 4.3.5 | MIT | Copyright (c) Tailwind Labs, Inc. | The parts of it that `make css` generates into `app.css` (its base styles and the utilities the pages use). The file keeps Tailwind's license banner. The Tailwind CLI that generates it is a build tool and is not shipped. |

Material adapted from another project, compiled into `deltav`:

| Component | Version | License | Copyright (from upstream files) | Shipped as |
|---|---|---|---|---|
| [tollbooth](https://github.com/blakegolliher/tollbooth) | 0.1.0 | Apache-2.0 | Copyright 2026 Blake Golliher | The table by which an S3 request is recognized as an operation (`internal/ratelimit/ops.go`), adapted from tollbooth's `internal/s3id/classify.go` and restated as data. tollbooth's `LICENSE` and `NOTICE` are under `third_party/static/tollbooth/`. |
