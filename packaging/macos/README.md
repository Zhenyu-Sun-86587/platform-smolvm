# Sandbox Platform on macOS

## Terminal onboarding

Run `sandboxctl` in an interactive terminal, or explicitly run `sandboxctl shell`. The shell shows a short welcome and current workspaces and snapshots. Type the same commands as in the one-shot CLI, omitting `sandboxctl`. Tab completes an unambiguous command or option. Up and Down recall commands for the current session. Type `exit` or press Ctrl+D to leave.

On first launch, choose whether to sign in. The interactive `sandboxctl login` asks for the Platform origin and offers to open its API Keys screen. Paste a token at the hidden prompt. A named profile can be selected with `--profile NAME`. The first-run prompt saves the profile without adding OpenSSH aliases. Run `sandboxctl login` directly to install the aliases described in the [CLI workflows](../../cmd/sandboxctl/USAGE.md).

Type `machines create` or `machine new` in the shell to choose a registered template and name. An explicit `machines create NAME --template TEMPLATE` runs through the same command tree. Commands such as `ssh` and `exec --tty` receive the terminal directly once launched. Bare non-interactive and `--json` invocations display help. Existing pipeline login with `login --token-stdin` and one-shot JSON commands still use their existing contract.

Press any key during the welcome to skip it. For a static welcome use `--no-animation` or `SANDBOXCTL_REDUCE_MOTION=1`. The CLI also checks the macOS Reduce Motion preference if it is available. Narrow and dumb terminals get a static welcome.

## Menu bar

On macOS 13 or newer with Go and Xcode Command Line Tools installed, run the following from the repository root.

```sh
packaging/macos/build.sh
open ".build/macos/Sandbox Menu.app"
```

Once Launch Services recognizes the app, `sandboxctl menubar` opens it. Quit the app before relaunching with another `--profile` or `--config-dir`. The bundle contains the matching `sandboxctl` helper. It reuses the selected CLI profile, including its origin and configured CA. **Sign In in Terminal** opens the same login flow.

The menu lists reported workspace states and opens a template picker to create a workspace. Select a workspace for Terminal, its website, Console, and applicable power actions. Managed Terminal uses the scoped capability reported for the selected machine. The enrolled-key action uses the legacy published SSH service. **Open Website** opens the Platform's HTTPS machine URL in the default browser. The Platform uses the browser's Console session for private website access, so the browser may ask the user to sign in. The Terminal launcher passes the selected profile and machine ID. The helper loads the token from the profile.

The build produces an ad hoc signed development bundle on the current architecture. Set `SANDBOX_MENU_SIGN_IDENTITY` to a local Developer ID Application signing identity for distribution signing. The script verifies the nested CLI and app signatures with `codesign`. A distribution still needs its own notarization. Apple documents the [bundle layout](https://developer.apple.com/documentation/bundleresources/placing-content-in-a-bundle) and [distribution signing](https://developer.apple.com/documentation/xcode/creating-distribution-signed-code-for-the-mac).

The macOS client contains the remote management commands. The Linux build includes local server administration, runtime diagnostics, and upgrade commands.

## Prototype branding and reference sources

The terminal welcome and menu icon both support an optional brand directory. Set `SANDBOXCTL_BRAND_DIR`, or use the `brand` subdirectory of the CLI config directory. The default macOS location is `~/Library/Application Support/sandboxctl/brand`. Put the following authorized resources there.

| File | Accepted content |
| --- | --- |
| `terminal.txt` | A terminal wordmark mask with spaces and `#`, rendered as full blocks. A maximum of eight rows and 80 columns fits the banner. |
| `menu-icon.png` | A transparent glyph for the macOS status item, rendered as a template image at 16 points. |

The default text uses the existing Sandbox Platform name and the menu uses Apple's shipping-box symbol. The short left-to-right reveal uses 15 frames at 40 ms each with a per-cell offset. Its timing and shade progression were observed in Boxd's public installer. The Go animation is implemented here. [Official installer](https://boxd.sh/downloads/install.sh)

| Reference | What it provided |
| --- | --- |
| [Boxd CLI & REPL](https://boxd.sh/features/cli) and [installation guide](https://docs.boxd.sh/cli/installation) | Bare-command REPL, workspace and snapshot inventory, one-shot command completion, and the installation recording. |
| [Boxd menu bar guide](https://docs.boxd.sh/guides/menu-bar) | Original menu-bar recording, machine status and actions, and shared login behavior. |
| [Official favicon](https://boxd.sh/favicon.svg), [docs wordmark](https://mintcdn.com/azin/psRZbQh2QwMUVL3U/images/logo-with-text-dark.svg), and [installer](https://boxd.sh/downloads/install.sh) | Boxd's public icon, graphical wordmark, and the original five-row terminal mask inside the install script. No asset reuse license was found. |
| [Boxd public repositories](https://github.com/boxd-sh), [MIT SDK](https://github.com/boxd-sh/sdk/blob/main/LICENSE), and [self-host installer license](https://github.com/boxd-sh/self-host/blob/main/LICENSE) | The MIT SDK and Apache-2.0 installer target other Boxd interfaces. The CLI, menu app and brand assets were outside the public source trees reviewed. Boxd lists [separate binary terms](https://github.com/boxd-sh/self-host/blob/main/LICENSE-BINARIES.md). |
| Local `docs/SMOLVM_PHASE2_POLISH_PACKAGE_2026-11-26.zip` | The backend CLI product report, captured command documentation, self-host licensing notes, and frontend reference materials. The archive contained console captures and CLI observations. The installer and menu recordings were sourced from the official references above. |

The branch packages the brand inputs and independently implemented animation only. The official downloads, the extracted terminal mask, and their provenance are available for internal review in the workspace build evidence directory.
