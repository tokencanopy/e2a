# Bundled fonts

The web application bundles the following Latin variable fonts so production
builds do not depend on a live Google Fonts response:

| Font | Package | Version | License | Source |
| --- | --- | --- | --- | --- |
| Inter | `@fontsource-variable/inter` | 5.3.0 | SIL Open Font License 1.1 | https://fontsource.org/fonts/inter |
| JetBrains Mono | `@fontsource-variable/jetbrains-mono` | 5.3.0 | SIL Open Font License 1.1 | https://fontsource.org/fonts/jetbrains-mono |
| Fraunces | `@fontsource-variable/fraunces` | 5.3.0 | SIL Open Font License 1.1 | https://fontsource.org/fonts/fraunces |

The corresponding license texts are in `licenses/`. The checked-in WOFF2
files are the Latin variable-font artifacts from those packages; their package
metadata and source URLs are recorded above for reproducibility.
