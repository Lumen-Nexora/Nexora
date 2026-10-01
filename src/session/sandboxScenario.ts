import { createTestClock, type TestClock } from "./testClock";

export type SandboxSettlementState = "pending" | "expired" | "settled";

export interface SandboxScenario {
  clock: TestClock;
  expiresAt: Date;
  state(): SandboxSettlementState;
  settle(): void;
}

export function createSandboxScenario(ttlMs: number, initial = new Date(0)): SandboxScenario {
  if (!Number.isFinite(ttlMs) || ttlMs <= 0) throw new RangeError("ttlMs must be positive");
  const clock = createTestClock(initial);
  const expiresAt = new Date(initial.getTime() + ttlMs);
  let settled = false;
  return {
    clock,
    expiresAt,
    state: () => settled ? "settled" : clock.now() >= expiresAt ? "expired" : "pending",
    settle: () => {
      if (clock.now() >= expiresAt) throw new Error("expired sandbox settlements cannot be settled");
      settled = true;
    },
  };
}
