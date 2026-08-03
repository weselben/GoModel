<script>
  // Skeleton placeholder for the Models table during the initial fetch.
  // Rendered only while `loading && displayModels.length === 0` (wired in
  // ModelsPage.svelte); refreshes keep the LoadingState spinner pill.
  // Six shimmer rows mirror the ModelTable shape with a fixed generic
  // column set — per-category columns are unnecessary for a placeholder.
</script>

<div
  class="table-wrapper"
  role="status"
  aria-live="polite"
  aria-label="Loading models"
>
  <table class="data-table">
    <thead>
      <tr>
        <th>Model</th>
        <th>Provider</th>
        <th class="col-price">Pricing</th>
        <th class="model-actions-header"></th>
      </tr>
    </thead>
    <tbody>
      {#each [1, 2, 3, 4, 5, 6] as _, i (i)}
        <tr>
          <td aria-hidden="true"><span class="skeleton-bar skeleton-bar-model"></span></td>
          <td aria-hidden="true"><span class="skeleton-bar skeleton-bar-provider"></span></td>
          <td aria-hidden="true"><span class="skeleton-bar skeleton-bar-price"></span></td>
          <td aria-hidden="true"><span class="skeleton-bar skeleton-bar-actions"></span></td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>

<style>
  .skeleton-bar {
    display: block;
    height: 14px;
    border-radius: var(--radius);
    background: linear-gradient(
      90deg,
      var(--bg) 25%,
      var(--bg-surface-hover) 50%,
      var(--bg) 75%
    );
    background-size: 200% 100%;
    animation: skeleton-shimmer 1.4s ease-in-out infinite;
  }

  .skeleton-bar-model {
    width: 52%;
  }

  /* Vary the model-name bar per row so the placeholder reads as staggered
     content instead of six identical rows. */
  tr:nth-child(2n) .skeleton-bar-model {
    width: 38%;
  }

  tr:nth-child(3n) .skeleton-bar-model {
    width: 64%;
  }

  .skeleton-bar-provider {
    width: 72px;
  }

  .skeleton-bar-price {
    width: 56px;
    margin-left: auto;
  }

  .skeleton-bar-actions {
    width: 84px;
    margin-left: auto;
  }

  /* Freeze the shimmer (single static frame) for reduced-motion users,
     matching the convention in src/styles/cards-charts.css. */
  @media (prefers-reduced-motion: reduce) {
    .skeleton-bar {
        animation: none;
      }
  }

  @keyframes skeleton-shimmer {
    0% {
      background-position: 200% 0;
    }
    100% {
      background-position: -200% 0;
    }
  }
</style>
