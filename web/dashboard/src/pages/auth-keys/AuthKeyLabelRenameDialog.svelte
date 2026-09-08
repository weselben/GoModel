<script>
  // Rename-one-label-everywhere dialog: shows the current label, the number
  // of keys affected, and a single new-name field.
  import EditorDialog from "$lib/components/organisms/EditorDialog.svelte";
  import FormField from "$lib/components/molecules/FormField.svelte";
  import { authKeysStore as store } from "./authKeys.svelte.js";
  import * as m from "$lib/paraglide/messages.js";

  // The fetched key list still reflects the pre-rename state while the
  // dialog is open, so the count stays stable across the edit.
  function affectedCount(label) {
    const item = store.distinctLabels.find((entry) => entry.label === label);
    return item ? item.count : 0;
  }
</script>

<EditorDialog
  open={store.labelRename.open}
  title={m.api_keys_label_rename_title({ label: store.labelRename.from })}
  ariaLabel={m.api_keys_label_rename_dialog({ label: store.labelRename.from })}
  error={store.labelRename.error}
  submitting={store.labelRename.submitting}
  submitLabel={m.api_keys_label_rename_submit({ label: store.labelRename.from })}
  dialogClass="auth-key-editor"
  onclose={() => store.closeLabelRename()}
  onsubmit={() => store.submitLabelRename()}
>
  <FormField id="auth-key-label-rename-to" label={m.api_keys_label_rename_to_label()}>
    <input
      id="auth-key-label-rename-to"
      type="text"
      placeholder={store.labelRename.from}
      autocomplete="off"
      data-modal-autofocus
      bind:value={store.labelRename.to}
    />
    <p class="form-hint">
      {m.api_keys_label_rename_help({
        label: store.labelRename.from,
        count: affectedCount(store.labelRename.from),
      })}
    </p>
  </FormField>
</EditorDialog>
