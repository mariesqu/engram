# Third-party assets

The engram binary embeds a small set of third-party assets so the web UI is
fully self-hosted (the daemon's own CSP is `default-src 'self'` — no CDN). Each
asset keeps its upstream license text next to it, and every license file is
embedded in the binary along with the asset it covers.

| Asset | Version | License | License file |
|-------|---------|---------|--------------|
| [JetBrains Mono](https://github.com/JetBrains/JetBrainsMono) (`JetBrainsMono-Regular.ttf`, `JetBrainsMono-Bold.ttf`) | 2.305 | SIL Open Font License 1.1 | [`internal/webui/static/fonts/OFL-JetBrainsMono.txt`](internal/webui/static/fonts/OFL-JetBrainsMono.txt) |
| [Lora](https://github.com/cyrealtype/Lora-Cyrillic) (`Lora-Italic.ttf`) | 3.008 | SIL Open Font License 1.1 | [`internal/webui/static/fonts/OFL-Lora.txt`](internal/webui/static/fonts/OFL-Lora.txt) |
| [DM Serif Display](https://github.com/googlefonts/dm-fonts) (`DMSerifDisplay-Italic.ttf`) | 5.200 | SIL Open Font License 1.1 | [`internal/webui/static/fonts/OFL-DMSerifDisplay.txt`](internal/webui/static/fonts/OFL-DMSerifDisplay.txt) |
| [htmx](https://github.com/bigskysoftware/htmx) (`htmx.min.js`) | 2.0.4 | Zero-Clause BSD (0BSD) | [`internal/webui/static/htmx.LICENSE.txt`](internal/webui/static/htmx.LICENSE.txt) |

Go module dependencies are listed in [`go.mod`](go.mod) and carry their own
licenses; they are not vendored in this repository.
