<script>
  // Mass label management for API keys: every label in use, with the number
  // of keys carrying it and a rename-across-all-keys action per label.
  import Icon from "$lib/components/atoms/Icon.svelte";
  import { authKeysStore as store } from "./authKeys.svelte.js";
  import { labelChipStyle } from "./authKeysLogic.js";
  import { Pencil } from "lucide";
  import * as m from "$lib/paraglide/messages.js";
</script>

{#if store.distinctLabels.length > 0 && store.available}
  <section class="auth-key-label-manager" aria-label={m.api_keys_label_manager_title()}>
    <h3 class="auth-key-label-manager-title">{m.api_keys_label_manager_title()}</h3>
    <p class="form-hint">{m.api_keys_label_manager_help()}</p>
    <ul class="auth-key-label-manager-list">
      {#each store.distinctLabels as item (item.label)}
        <li class="auth-key-label-manager-item">
          <span class="usage-label-chip usage-label-chip-static" style={labelChipStyle(item.label)}>
            {item.label}
          </span>
          <span class="auth-key-label-manager-count">
            {m.api_keys_label_key_count({ count: item.count })}
          </span>
          <button
            type="button"
            class="btn auth-key-label-manager-rename"
            title={m.api_keys_label_rename_action({ label: item.label })}
            onclick={() => store.openLabelRename(item.label)}
          >
            <Icon icon={Pencil} class="table-icon-svg" />
            <span>{m.api_keys_label_rename()}</span>
          </button>
        </li>
      {/each}
    </ul>
  </section>
{/if}

<style>
.auth-key-label-manager {
  margin-top: 24px;
}

.auth-key-label-manager-title {
  margin: 0 0 6px;
  font-size: 15px;
}

.auth-key-label-manager-list {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  margin: 12px 0 0;
  padding: 0;
  list-style: none;
}

.auth-key-label-manager-item {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--bg);
}

.auth-key-label-manager-count {
  font-size: 12px;
  color: var(--text-muted);
}

.auth-key-label-manager-rename {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 3px 8px;
  font-size: 12px;
}
</style>
