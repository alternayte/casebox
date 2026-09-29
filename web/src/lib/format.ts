export type Rate = { value: number; n: number; interval: [number, number] };

export function pct(share: number, digits = 0): string {
  return `${(share * 100).toFixed(digits)}%`;
}

export function interval(i: [number, number]): string {
  return `${pct(i[0])}–${pct(i[1])}`;
}

export function num(n: number): string {
  return n.toLocaleString("en-GB");
}

// Days only: the UI never shows a time finer than a day for captured work.
export function day(iso: string): string {
  return iso.slice(0, 10);
}

export function isoDay(d: Date): string {
  return d.toISOString().slice(0, 10);
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${num(n)} ${n === 1 ? one : many}`;
}
