# What changed in each release

Every release of exe and what it told its users had changed, newest first.
These are the notes Software Update shows before **Update Now** is pressed.
A release's page at https://github.com/livid/exe/releases says more: what
was tried before it went out, and what to know before installing.

`deploy/release.sh publish` adds a release here when it is out. What the
next one will say is in [NOTES.md](NOTES.md).

## 2026.10.12

- **Software Update** in the Apple menu installs a new release from the desktop.
- **Welcome…** in the Help menu is exe in six cards. **Extras…** on its Apps card turns extra apps on and off.
- **Join a Hub…** in the Hub app: four steps, from a hub's address to your profile.
- **Node Sync…** in the Special menu is the old **Join…**: it pairs this exe with another.
- **Mac OS X** in the Special menu: a Power Mac G4 running Mac OS X 10.4 Tiger, beside Mac OS 9.
- **Search**, beside **New** in the Claude Code window, finds any conversation, archived ones too.
- Right-click the empty part of a Workspace window for **View**: **as Icons** or **as List**, kept per folder.
- Right-click a window's title bar for **Save a Screenshot**: a PNG of it lands in Workspace/Screenshots.

## 2026.10.10.2

This release and the one before it carried no notes: these were written afterwards, from its page.

- **Windows.** One line in PowerShell installs exe: `irm https://exe.v2core.com/install.ps1 | iex`.
- **`vm_dir`** in the configuration says where the VMs' disks and the base images are kept.
- **`exe daemon start`** and **`exe daemon restart`** start and restart an installed exe by hand.

## 2026.10.10

The first release. Until then exe was built from a checkout.

- One line installs exe on Linux and macOS: `curl -fsSL https://exe.v2core.com/install.sh | sh`.
- **`exe update`** moves an installed exe to the next release. **`exe uninstall`** removes it and keeps your data.
