import test from "node:test";
import assert from "node:assert/strict";

import { shouldShowModelsSkeleton } from "../src/pages/models/modelsSkeletonLogic.js";

test("shouldShowModelsSkeleton shows during initial fetch with no rows", () => {
  assert.equal(shouldShowModelsSkeleton(true, 0), true);
});

test("shouldShowModelsSkeleton hides once any row has arrived, even on refresh", () => {
  assert.equal(shouldShowModelsSkeleton(true, 1), false);
  assert.equal(shouldShowModelsSkeleton(true, 42), false);
});

test("shouldShowModelsSkeleton hides when not loading and no rows are present", () => {
  assert.equal(shouldShowModelsSkeleton(false, 0), false);
});

test("shouldShowModelsSkeleton hides when not loading and rows are present", () => {
  assert.equal(shouldShowModelsSkeleton(false, 7), false);
});
