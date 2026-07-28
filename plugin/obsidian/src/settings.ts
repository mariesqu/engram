import { PluginSettingTab, Setting } from "obsidian";
// Type-only import: erased at compile time by TypeScript, so this does NOT
// create a runtime circular dependency with main.ts (main.ts imports this
// file's EngramSettingTab as a value). This is the same pattern the
// official Obsidian sample plugin uses.
import type EngramBrainPlugin from "./main";

/**
 * The plugin's entire persisted configuration. Three fields survive the
 * transport re-decision (decision/obsidian-plugin-transport, REQ-PLUGIN-02)
 * -- everything else the recovered plan had (engramUrl, engramBinary,
 * databasePath, autoSyncMinutes, lastSyncAt/lastSyncCount, a bearer token)
 * is retired because the plugin no longer syncs, spawns, or opens a
 * database or a socket of any kind.
 */
export interface EngramSettings {
  /** Vault-relative folder the exporter writes into; the folder this plugin READS. */
  vaultSubfolder: string;
  /** DISPLAY scope only -- restricts the counts shown, never what the exporter exports. */
  projectFilter: string;
  /** Age (minutes) past which the vault is reported stale (REQ-PLUGIN-05). */
  staleAfterMinutes: number;
}

export const DEFAULT_SETTINGS: EngramSettings = {
  vaultSubfolder: "engram",
  projectFilter: "",
  staleAfterMinutes: 60,
};

/**
 * Renders a Date as a short, human relative string ("2 minutes ago"). Never
 * throws; a null input (freshness unknown) renders as "unknown" rather than
 * "NaN minutes ago" or similar.
 */
export function formatRelative(date: Date | null, now: Date = new Date()): string {
  if (!date) {
    return "unknown";
  }

  const diffMs = now.getTime() - date.getTime();
  if (diffMs < 0) {
    // Clock skew between this device and whatever wrote the timestamp --
    // render as "just now" rather than a confusing negative duration.
    return "just now";
  }

  const diffSec = Math.round(diffMs / 1000);
  if (diffSec < 45) {
    return "just now";
  }

  const diffMin = Math.round(diffSec / 60);
  if (diffMin < 60) {
    return `${diffMin} minute${diffMin === 1 ? "" : "s"} ago`;
  }

  const diffHour = Math.round(diffMin / 60);
  if (diffHour < 24) {
    return `${diffHour} hour${diffHour === 1 ? "" : "s"} ago`;
  }

  const diffDay = Math.round(diffHour / 24);
  return `${diffDay} day${diffDay === 1 ? "" : "s"} ago`;
}

export class EngramSettingTab extends PluginSettingTab {
  plugin: EngramBrainPlugin;

  constructor(app: import("obsidian").App, plugin: EngramBrainPlugin) {
    super(app, plugin);
    this.plugin = plugin;
  }

  display(): void {
    const { containerEl } = this;
    containerEl.empty();

    containerEl.createEl("h2", { text: "Engram Brain" });
    containerEl.createEl("p", {
      text:
        "This plugin does not sync anything. A separate process -- the engram " +
        "daemon's scheduled Obsidian export -- keeps this vault up to date on " +
        "its own cadence. This plugin only reports how fresh what is already " +
        "on disk is.",
    });

    new Setting(containerEl)
      .setName("Vault subfolder")
      .setDesc(
        "The vault-relative folder the exporter writes into. The exporter's " +
          'namespace is currently hard-coded to "engram/", so "engram" is the ' +
          "only value that will ever find an export. A wrong value degrades to " +
          '"no export found" -- it does not raise an error.',
      )
      .addText((text) =>
        text
          .setPlaceholder("engram")
          .setValue(this.plugin.settings.vaultSubfolder)
          .onChange(async (value) => {
            this.plugin.settings.vaultSubfolder = value.trim() || DEFAULT_SETTINGS.vaultSubfolder;
            await this.plugin.saveSettings();
          }),
      );

    new Setting(containerEl)
      .setName("Project filter")
      .setDesc(
        "Restricts the COUNTS DISPLAYED by this plugin only. It has no effect " +
          "on what the exporter actually exports -- that scope is controlled by " +
          "the daemon's own obsidian_project setting. Leave empty to show every " +
          "project.",
      )
      .addText((text) =>
        text
          .setPlaceholder("(all projects)")
          .setValue(this.plugin.settings.projectFilter)
          .onChange(async (value) => {
            this.plugin.settings.projectFilter = value.trim();
            await this.plugin.saveSettings();
          }),
      );

    new Setting(containerEl)
      .setName("Stale after (minutes)")
      .setDesc("Age past which the vault is reported stale in the status bar.")
      .addText((text) =>
        text
          .setPlaceholder(String(DEFAULT_SETTINGS.staleAfterMinutes))
          .setValue(String(this.plugin.settings.staleAfterMinutes))
          .onChange(async (value) => {
            const parsed = Number.parseInt(value, 10);
            this.plugin.settings.staleAfterMinutes =
              Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_SETTINGS.staleAfterMinutes;
            await this.plugin.saveSettings();
          }),
      );

    const status = this.plugin.freshness;
    const statusText = status
      ? status.ok
        ? `${status.noteCount} notes, last export ${formatRelative(status.lastExportAt)} (${status.statePath})`
        : `no export found (looked for ${status.statePath}). Run "engram config set obsidian_vault <vault>" and restart the daemon, or run "engram obsidian-export --db <path> --vault <vault>" once to populate it now.`
      : "not checked yet";

    new Setting(containerEl)
      .setName("Current status")
      .setDesc(statusText)
      .addButton((button) =>
        button.setButtonText("Refresh now").onClick(async () => {
          await this.plugin.refresh();
          this.display();
        }),
      );
  }
}
