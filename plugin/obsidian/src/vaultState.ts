import type { App } from "obsidian";
import type { EngramSettings } from "./settings";

/**
 * VaultFreshness is the plugin's entire view of the world. It is derived
 * exclusively from a read-only parse of the exporter's own state file
 * ("{vaultSubfolder}/.engram-sync-state.json", written by
 * internal/obsidian/state.go) -- nothing here is computed by walking the
 * vault's file index, and nothing here ever writes anything back.
 *
 * `statePath` is not part of the design's originally sketched interface; it
 * is added here because REQ-PLUGIN-05 requires the hover tooltip to show
 * the resolved state-file path, and the caller has no other way to recover
 * it without duplicating the subfolder-join logic.
 */
export interface VaultFreshness {
  lastExportAt: Date | null;
  noteCount: number;
  projectCounts: Record<string, number>;
  ok: boolean;
  statePath: string;
}

// Shape of the JSON internal/obsidian/state.go's SyncState marshals. Field
// names are read off the SHIPPED Go struct (tasks 1.6 + 7.4), not assumed:
//
//   type SyncState struct {
//       LastExportAt time.Time         `json:"last_export_at"`
//       Files        map[int64]string  `json:"files"`
//       Hubs         map[string]string `json:"hubs"`
//   }
//
// `files` keys are observation IDs (int64, serialized as JSON string keys);
// values are vault-ROOT-relative paths of the form
// "engram/{project}/{type}/{slug}-{id}.md" (internal/obsidian/exporter.go's
// vaultRelPath -- the "engram/" segment is the hard-coded namespace, not
// `vaultSubfolder`; see REQ-PLUGIN-02). `hubs` is deliberately never read
// here: hub notes are navigation scaffolding, not observations, and REQ-
// PLUGIN-05 / task 4.4 are explicit that counting them would inflate the
// number the status bar calls "notes".
interface RawSyncState {
  last_export_at?: string;
  files?: Record<string, string>;
  hubs?: Record<string, string>;
}

function absent(statePath: string): VaultFreshness {
  return { lastExportAt: null, noteCount: 0, projectCounts: {}, ok: false, statePath };
}

/**
 * Reads and parses the exporter's sync-state file and derives the freshness
 * summary the status bar and settings tab render. Never throws: every
 * failure mode -- missing file, unreadable file, malformed JSON, a missing
 * or unparseable timestamp -- degrades to the Absent state (spec
 * REQ-PLUGIN-09) rather than raising, so a single bad read can never break
 * the status bar for the rest of the session.
 */
export async function readVaultState(app: App, settings: EngramSettings): Promise<VaultFreshness> {
  const subfolder = (settings.vaultSubfolder || "engram").trim().replace(/^\/+|\/+$/g, "");
  const statePath = `${subfolder}/.engram-sync-state.json`;

  // DESIGN CONSTRAINT 13 -- LOAD-BEARING, UNVERIFIED ASSUMPTION.
  //
  // `.engram-sync-state.json` is dot-prefixed. Obsidian's INDEXED Vault API
  // (app.vault.getAbstractFileByPath, app.vault.getFiles, the "raw" vault
  // event, etc.) is documented and widely reported to exclude dot-prefixed
  // files from its index. If that holds, looking this file up through the
  // indexed API would return null against a perfectly healthy, freshly
  // exported vault, and this plugin would report "no export found" forever
  // -- a permanent false negative, not a crash.
  //
  // `app.vault.adapter` is the RAW filesystem adapter that sits underneath
  // the index. It is not indexed and has no opinion about dotfiles, so this
  // module reads exclusively through it (`exists` / `read` / `stat`) and
  // never touches `app.vault.getAbstractFileByPath` or `app.vault.getFiles`.
  //
  // This assumption cannot be verified from this repository or by this
  // agent -- there is no Obsidian runtime available here. If it turns out
  // to be wrong, the observable failure is exactly the graceful one this
  // module already produces for "no export found": no crash, no thrown
  // exception, just a status bar that never leaves the Absent state.
  const adapter = app.vault.adapter;

  try {
    const exists = await adapter.exists(statePath);
    if (!exists) {
      return absent(statePath);
    }

    const raw = await adapter.read(statePath);

    let parsed: RawSyncState;
    try {
      parsed = JSON.parse(raw) as RawSyncState;
    } catch {
      // Truncated or half-written file -- e.g. this read landed mid-rename
      // while the exporter's atomic WriteState was in flight. Degrade for
      // THIS tick only; the 60s poll (REQ-PLUGIN-10) re-reads and recovers
      // on its own once the rename has landed, per REQ-PLUGIN-09.
      return absent(statePath);
    }

    const files = parsed.files ?? {};

    let lastExportAt: Date | null = null;
    if (typeof parsed.last_export_at === "string") {
      const fromField = new Date(parsed.last_export_at);
      if (!Number.isNaN(fromField.getTime())) {
        lastExportAt = fromField;
      }
    }
    if (lastExportAt === null) {
      // last_export_at absent or unparseable -- fall back to the state
      // file's own mtime rather than treating the whole read as failed.
      const stat = await adapter.stat(statePath);
      if (stat && typeof stat.mtime === "number") {
        lastExportAt = new Date(stat.mtime);
      }
    }

    if (lastExportAt === null) {
      // Neither the state file's own timestamp nor its filesystem mtime
      // could be resolved. There is no honest freshness claim left to make
      // here, so this degrades the same as "no export found" rather than
      // guessing (or worse, rendering as Fresh with an empty timestamp).
      return absent(statePath);
    }

    const projectFilter = (settings.projectFilter ?? "").trim();
    const projectCounts: Record<string, number> = {};
    let noteCount = 0;

    for (const relPath of Object.values(files)) {
      // "engram/{project}/{type}/{slug}-{id}.md" -- segment 0 is the
      // exporter's hard-coded namespace, segment 1 is the project.
      const segments = relPath.split("/");
      const project = segments.length >= 2 ? segments[1] : undefined;

      if (project) {
        projectCounts[project] = (projectCounts[project] ?? 0) + 1;
      }
      // projectCounts is always the FULL breakdown (for the tooltip);
      // noteCount is the DISPLAYED count, scoped by projectFilter when set
      // (REQ-PLUGIN-05: "restricted to projectFilter when set").
      if (!projectFilter || project === projectFilter) {
        noteCount += 1;
      }
    }

    return { lastExportAt, noteCount, projectCounts, ok: true, statePath };
  } catch {
    // Any other adapter failure -- permission error, a transient I/O error,
    // a rejected promise from a sync client holding the file open, etc.
    // Never throw out of this function; the caller (main.ts's refresh())
    // must never have to wrap this call in its own try/catch.
    return absent(statePath);
  }
}
