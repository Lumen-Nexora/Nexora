export interface TestClock { now(): Date; advance(milliseconds: number): Date; set(instant: Date): Date; }

export function createTestClock(initial: Date = new Date(0)): TestClock {
  let current = initial.getTime();
  return {
    now: () => new Date(current),
    advance: (milliseconds) => { if (!Number.isFinite(milliseconds) || milliseconds < 0) throw new RangeError("advance must be non-negative"); current += milliseconds; return new Date(current); },
    set: (instant) => { if (Number.isNaN(instant.getTime())) throw new RangeError("invalid clock instant"); current = instant.getTime(); return new Date(current); },
  };
}
