# Third-Party Notices / 第三方说明

WittyChat's AGPL-3.0-only license does not replace third-party licenses.
Dependencies and separately marked assets retain their own copyright and terms.

| Component | Source of version/license information |
| --- | --- |
| Go dependencies, including Wails, pgx, AWS SDK, Yjs relay dependencies and SQLite runtime | `go.mod`, `go.sum`, upstream module license/notice files |
| React, Vite, TipTap/ProseMirror, Yjs, DOMPurify and other JS packages | `admin/pnpm-lock.yaml`, `desktop/frontend/pnpm-lock.yaml`, package manifests and upstream notices |
| Inter font | `desktop/frontend/Inter Font License.txt` (SIL Open Font License 1.1) |
| Wails/React scaffold assets | Wails/React upstream repositories and their asset notices |
| Microsoft Edge WebView2 | Microsoft's runtime terms; installed separately, not bundled |
| PostgreSQL, Garage, OnlyOffice, maddy, Nginx and container base images | Respective upstream projects/container distributions; not included in the portable archive |

The release packaging script collects dependency license/notice files into
`third-party/` and includes dependency metadata. Keep that directory with binary
redistributions. License availability should be checked before introducing new
dependencies or distributing a different artifact.

Source for this release is available at its exact Git tag, together with build
scripts and dependency locks. GitHub automatically supplies source archives.
Do not assume all components share WittyChat's license or that a container's
commercial edition is covered by this repository.

第三方内容遵循各自许可证；发布包的 `third-party/` 不应被移除。
字体授权全文保留在仓库与发布包中。本说明不是免除许可证义务的声明。
