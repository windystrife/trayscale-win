Trayscale for Windows
=====================

A native **Windows** port of [Trayscale](https://github.com/DeedleFake/trayscale).

The upstream project is a GTK4/libadwaita GUI built for Linux and does not run on
Windows. This port keeps Trayscale's cross-platform core (`internal/tsutil`,
which talks to the Tailscale LocalAPI) and re-implements the UI with **pure-Go**
toolkits — no GTK, no CGO, no MSYS2. There are two editions:

| Binary | UI | Toolkit |
|--------|----|---------|
| `dist\Trayscale-GUI.exe` (~28 MB) | Full **window**: machine sidebar + detail pane with IPs and option toggles, reproducing the upstream libadwaita layout | [Gio](https://gioui.org) |
| `dist\Trayscale.exe` (~22 MB) | System-**tray** icon + menu | [`fyne.io/systray`](https://github.com/fyne-io/systray) |

Pick whichever you prefer — they are independent and use the same core.

GUI edition (window)
--------------------

`dist\Trayscale-GUI.exe` opens a window that mirrors the original Trayscale and
adds the owner-based grouping of the official Windows client:

* A left **navigation rail** with **Devices** and **Exit Nodes** views (like the
  macOS client).
* A **connect toggle**, your tailnet login, and the current **exit node** in the
  header.
* A **searchable sidebar** (type to filter) with machines **grouped by owner** —
  *This machine*, a *My devices* group, then one group per other user
  (`banganh`, `dangnguyen`, …), just like the official Windows menu. Each row
  shows the device's Tailscale IP; colored avatars mark online (green) / offline
  (red).
* A scrollable **detail pane** for the selected machine, combining the Linux and
  macOS layouts:
  * A **status dot** (Connected / Not Connected) under the name.
  * **Tailscale addresses** — MagicDNS name, IPv4 and IPv6, each labelled with a
    **Copy** button (macOS style).
  * **Options** (this machine) — *Advertise exit node*, *Allow LAN access*,
    *Accept routes*, *Accept DNS*, each with its description. For a peer that can
    be an exit node, a *Use as exit node* switch instead.
  * **Taildrop** (peers) — **Select a File…** sends a file to that device.
  * **Ping** (peers) — **Ping device** / **Stop** runs a continuous ping,
    showing the connection type (**Direct** green / **DERP-relayed** orange), the
    live latency, and a smoothed, auto-scaled latency graph (macOS-style).
  * **Files** (this machine) — pending Taildrop files (Delete), or "No incoming
    files."
  * **Advertised Routes** (this machine) — list with **+** to add and **Remove**.
  * **Network Check** (this machine) — a **↻** button runs a netcheck (UDP,
    IPv4/IPv6, preferred DERP, per-region latency).
  * **Details** — OS, key expiry, created, last seen.
* The **Exit Nodes** view lists exit-capable peers with *None*, a checkmark on the
  active one, and an *Allow local network access* toggle.

Tray edition
------------

From the tray icon you can:

* See connection state at a glance — the icon changes for **disconnected**,
  **connected**, and **connected via exit node**.
* **Connect / Disconnect** (`tailscale up` / `down`), or **Log in** when needed.
* Pick an **exit node** from the list of exit-capable peers, or set it to *None*.
* Toggle **Allow local network access**, **Run as exit node**, **Accept subnet
  routes**, and **Accept DNS**.
* Browse **Peers** and click one to copy its Tailscale IP to the clipboard.
* Copy **this machine's** Tailscale IP (click the top menu entry or left-click
  the tray icon for a status summary).
* Open the **Admin console** in your browser.

Requirements
------------

* Windows 10 / 11 (x64).
* The official **Tailscale for Windows** installed and signed in
  (`tailscaled` runs as a service). Download: <https://tailscale.com/download/windows>.

The app talks to the running Tailscale service over its LocalAPI as the current
user — no elevation is needed for reading status or for the usual
connect/disconnect/exit-node actions.

Install / Run
-------------

Prebuilt binaries are in `dist\`. Double-click **`Trayscale-GUI.exe`** for the
window, or **`Trayscale.exe`** for the tray icon (you may need to expand the
hidden-icons `^` area and drag it onto the taskbar). Quit the tray edition from
its menu → **Quit**; close the window edition with its title-bar ✕.

### Start automatically with Windows

Put a shortcut in your Startup folder (swap the exe name for whichever edition
you want to autostart):

```powershell
$exe = "H:\Claude\trayscale\dist\Trayscale-GUI.exe"   # or dist\Trayscale.exe
$startup = [Environment]::GetFolderPath('Startup')
$s = (New-Object -ComObject WScript.Shell).CreateShortcut("$startup\Trayscale.lnk")
$s.TargetPath = $exe
$s.WorkingDirectory = Split-Path $exe
$s.Save()
```

Remove it later by deleting `%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\Trayscale.lnk`.

Build from source
-----------------

Requires Go >= 1.26.5.

```powershell
powershell -ExecutionPolicy Bypass -File .\build-windows.ps1
```

This regenerates each edition's embedded Windows manifest (`rsrc_windows_amd64.syso`,
via `github.com/akavel/rsrc`) if needed and produces both `dist\Trayscale-GUI.exe`
and `dist\Trayscale.exe`.

> **The manifest is required.** Tailscale's Windows version detection
> (`github.com/dblohm7/wingoes`) panics with *"incoherent Windows version --
> missing/outdated manifest"* unless the executable embeds an application
> manifest declaring `supportedOS` for Windows 10/11. See
> `cmd\trayscale-win\app.manifest`.

Logs
----

Runtime logs (state changes, action errors) are written to:

```
%APPDATA%\Trayscale\trayscale-gui.log   (window edition)
%APPDATA%\Trayscale\trayscale.log       (tray edition)
```

Notes & limitations
-------------------

* Mullvad exit nodes are omitted from the lists to keep them short. The tray's
  exit-node and peer lists are capped (50 / 100 entries).
* Taildrop (send/receive files), NetCheck, and multi-profile switching from the
  original UI are not yet exposed.
* The window edition's title bar uses standard Windows decorations rather than
  the upstream client-side (libadwaita) header bar.

Layout of the Windows-specific code
-----------------------------------

| Path | Purpose |
|------|---------|
| `cmd/trayscale-gui/` | Window entry point + `app.manifest` + generated `.syso` |
| `internal/winui/`    | Gio window — sidebar + detail pane, wired to `tsutil` |
| `cmd/trayscale-win/` | Tray entry point + `app.manifest` + generated `.syso` |
| `internal/wintray/`  | Tray icon + menu, wiring the `tsutil` core to systray |
| `internal/tsutil/`   | Unchanged upstream core — Tailscale LocalAPI client + poller |
| `build-windows.ps1`  | One-shot build script (both editions) |
