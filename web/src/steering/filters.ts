import { groupBys, type GroupBy } from "@/lib/labels";
import { filterKeys, type Filters } from "./types";

const dayPattern = /^\d{4}-\d{2}-\d{2}$/;

// Reads the report filters from the URL; anything malformed is dropped.
export function validateFilters(search: Record<string, unknown>): Filters {
  const out: Filters = {};
  for (const key of filterKeys) {
    const value = search[key];
    if (typeof value !== "string" || value === "") continue;
    if ((key === "from" || key === "to") && !dayPattern.test(value)) continue;
    if (key === "groupBy") {
      if ((groupBys as readonly string[]).includes(value)) out.groupBy = value as GroupBy;
      continue;
    }
    out[key] = value;
  }
  return out;
}

// The filters a theme page carries over: everything but groupBy.
export function scopeOf(f: Filters): Filters {
  const { groupBy: _groupBy, ...rest } = f;
  return rest;
}
