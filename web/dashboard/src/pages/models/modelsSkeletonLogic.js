// Pure predicate backing the Models page skeleton loader.
//
// Returns true only when the models store is mid-fetch on the *initial*
// load — i.e. no rows have arrived yet. Once any model lands we stop
// showing the skeleton and let real rows (and the existing spinner pill)
// take over, even if a refresh keeps `loading` true.

export function shouldShowModelsSkeleton(loading, displayCount) {
  return loading && displayCount === 0;
}
