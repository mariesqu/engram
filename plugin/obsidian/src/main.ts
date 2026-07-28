import { Notice, Plugin } from "obsidian";
import { DEFAULT_SETTINGS, EngramSettingTab, EngramSettings, formatRelative } from "./settings";
import { readVaultState, VaultFreshness } from "./vaultState";

// manifest.json's "isDesktopOnly": true is retained here as a SCOPE
// decision, not a technical one (REQ-PLUGIN-01). This plugin needs no
// Node/Electron capability of any kind -- it reads one JSON file through
// Obsidian's vault adapter and writes nothing. Desktop-only survives for
// two unrelated reasons instead: (1) the scheduled export that keeps this
// vault fresh is a Go daemon process, which cannot run on Obsidian mobile
// at all, and (2) the state file this plugin reads is dot-prefixed
// (".engram-sync-state.json"), and a hidden dotfile is not reliably
// replicated to mobile by Obsidian Sync. Flipping the flag later, if
// mobile sync of dotfiles ever becomes reliable, is a manifest-only change
// -- nothing in this source depends on it.

/**
 * Engram Brain -- a read-only freshness viewer.
 *
 * This plugin does NOT sync. It has no child_process, no fetch/requestUrl,
 * no bearer token, no Engram URL setting, and no write path into the vault
 * beyond its own settings (Plugin.saveData(), REQ-PLUGIN-08). A separate,
 * already-in-scope mechanism -- the engram daemon's scheduled Obsidian
 * export -- is what keeps the vault current; this plugin only reports how
 * fresh what is already on disk is (decision/obsidian-plugin-transport).
 */
export default class EngramBrainPlugin extends Plugin {
  settings!: EngramSettings;
  freshness: VaultFreshness | null = null;

  private statusBarItemEl!: HTMLElement;
  // Guards against two overlapping reads if a manual refresh (command or
  // settings-tab button) fires while the 60s interval tick is already
  // mid-read (REQ-PLUGIN-10: "MUST NOT queue overlapping reads").
  private refreshing = false;
  // At most one Notice per Obsidian session PER FAILURE EPISODE
  // (REQ-PLUGIN-09) -- not a strict single Notice for the entire session.
  // The latch is set on a failed read and cleared on the next SUCCESSFUL
  // one, so a vault that recovers and later fails again (e.g. the daemon
  // is stopped mid-session) still gets exactly one Notice for that new
  // episode, rather than staying silent forever after the first one ever.
  private notifiedAbsent = false;

  async onload(): Promise<void> {
    await this.loadSettings();
    this.addSettingTab(new EngramSettingTab(this.app, this));

    this.statusBarItemEl = this.addStatusBarItem();
    this.statusBarItemEl.addClass("status-bar-item");

    // One immediate read so the bar is correct as soon as Obsidian finishes
    // loading -- REQ-PLUGIN-10. Deliberately awaited (not fire-and-forget):
    // it is a single small JSON read, not a network call, so there is no
    // meaningful startup cost to waiting for it, and awaiting it means the
    // bar never has to render a placeholder/"checking…" state, which the
    // spec forbids as a disguised progress indicator.
    await this.refresh();

    // Fixed 60s cadence, deliberately NOT a setting (REQ-PLUGIN-10):
    // registerInterval() hands the interval id to Obsidian, which clears it
    // automatically on unload -- there is nothing for onunload() to do.
    this.registerInterval(window.setInterval(() => void this.refresh(), 60_000));

    this.addCommand({
      id: "engram-refresh-status",
      name: "Engram: refresh vault status",
      callback: () => void this.refresh(),
    });
  }

  onunload(): void {
    // Nothing to tear down. registerInterval()'s interval is released by
    // Obsidian automatically, and this plugin never opens a subprocess or a
    // socket that would need closing (REQ-PLUGIN-01/-08) -- this method is
    // intentionally empty, stated explicitly so the empty body is not
    // mistaken for an oversight by a future reader.
  }

  async loadSettings(): Promise<void> {
    this.settings = Object.assign({}, DEFAULT_SETTINGS, await this.loadData());
  }

  async saveSettings(): Promise<void> {
    // The ONLY write this plugin ever performs (REQ-PLUGIN-08): Obsidian's
    // own per-plugin data.json (.obsidian/plugins/engram-brain/data.json),
    // holding nothing but the three settings above. Nothing inside the
    // vault's content tree is ever touched.
    await this.saveData(this.settings);
  }

  /**
   * Re-reads the exporter's state file and updates the status bar. Safe to
   * call from the interval tick, the command palette, or the settings tab's
   * "Refresh now" button -- overlapping calls collapse into a no-op via
   * `refreshing`, and `readVaultState` itself never throws.
   */
  async refresh(): Promise<void> {
    if (this.refreshing) {
      return;
    }
    this.refreshing = true;
    try {
      const freshness = await readVaultState(this.app, this.settings);
      this.freshness = freshness;
      this.renderStatusBar(freshness);
    } finally {
      this.refreshing = false;
    }
  }

  private renderStatusBar(freshness: VaultFreshness): void {
    if (!freshness.ok || !freshness.lastExportAt) {
      this.statusBarItemEl.setText("Engram: no export found");
      this.statusBarItemEl.setAttribute(
        "aria-label",
        `No export found at ${freshness.statePath}.\n` +
          'Fix: run "engram config set obsidian_vault <vault>" then restart the daemon.\n' +
          '(Or run "engram obsidian-export --db <path> --vault <vault>" once to populate it right now.)',
      );
      if (!this.notifiedAbsent) {
        new Notice("Engram: no export found for this vault. See the Engram Brain settings for the fix.");
        this.notifiedAbsent = true;
      }
      return;
    }

    // A successful read clears the latch -- see the field comment above.
    this.notifiedAbsent = false;

    const ageMs = Date.now() - freshness.lastExportAt.getTime();
    const staleMs = this.settings.staleAfterMinutes * 60_000;
    const relative = formatRelative(freshness.lastExportAt);

    // Exactly three states, no spinner and no "syncing…" state -- nothing
    // is ever in progress (REQ-PLUGIN-05). A state file that parses with
    // zero entries still renders here as Fresh/Stale with "0 notes", never
    // as Absent.
    if (ageMs <= staleMs) {
      this.statusBarItemEl.setText(`Engram: ${freshness.noteCount} notes · updated ${relative}`);
    } else {
      this.statusBarItemEl.setText(`Engram: ${freshness.noteCount} notes · stale (${relative})`);
    }

    const breakdown =
      Object.entries(freshness.projectCounts)
        .map(([project, count]) => `${project}: ${count}`)
        .join(", ") || "(no projects)";

    this.statusBarItemEl.setAttribute(
      "aria-label",
      `Last export: ${freshness.lastExportAt.toISOString()}\n` +
        `Projects: ${breakdown}\n` +
        `State file: ${freshness.statePath}`,
    );
  }
}
