# Sandbox Console notices

The modified dashboard frontend is distributed under GNU AGPL version 3. The local console offers its corresponding source and notices in the navigation footer. Daytona is copyright its software authors, initially Daytona Platforms, Inc. The original notices and license accompany this distribution as DAYTONA-COPYRIGHT, DAYTONA-NOTICE and DAYTONA-LICENSE.

The source baseline is [daytonaio/daytona v0.190.0](https://github.com/daytonaio/daytona/tree/v0.190.0/apps/dashboard), commit 01c502bb1f1ff8f2885d0cd490e043736083dca8. On 2026-11-25, Sandbox Console adapted these files under apps/dashboard/src:

| Upstream | Local use |
| --- | --- |
| components/ui/button.tsx, dialog.tsx, dropdown-menu.tsx, empty.tsx, skeleton.tsx | Locally built UI primitives in src/daytona, with local utility imports |
| components/ui/sidebar.tsx | Sidebar menu and menu item primitives |
| components/ui/table.tsx and components/PageLayout.tsx | Table primitives and page title/intro, with local columns and navigation |
| index.css | Dashboard theme tokens in src/daytona/theme.css |
| components/sandboxes/SandboxState.tsx | Visual reference for the local status indicator using platform observation and operation states |

The views, SDK adapter, and interactions use this platform's API. boxd’s public demonstrations and operator-supplied console screenshots informed navigation and operation grouping. Its marketing assets are kept separately from the application.

The build collects installed third-party frontend dependency license texts in the notices download. These include React, Radix UI, Lucide, xterm, Inter, JetBrains Mono and their applicable licenses. To rebuild the web bundle from the source archive on Linux, run `cd web && npm ci && npm run build`. To compile the embedding executable, run `go build ./cmd/platformd` in the repository root. Existing component notices retain their own terms. The adapted shadcn/ui primitives include the MIT notice from [shadcn/ui](https://github.com/shadcn-ui/ui/blob/main/LICENSE.md). Compiled Tailwind and animation styles are included in the notices download.
