import { createBalanceSnapshot, type BalanceSnapshot, type WalletBalance } from "../wallets/snapshots";

export interface SnapshotStore {
  create(tenantId: string, cutoff: Date, balances: readonly WalletBalance[]): BalanceSnapshot;
  get(tenantId: string, cutoff: Date): BalanceSnapshot | undefined;
}

export function createSnapshotStore(): SnapshotStore {
  const snapshots = new Map<string, BalanceSnapshot>();
  const key = (tenantId: string, cutoff: Date) => tenantId + ":" + cutoff.toISOString();
  return {
    create(tenantId, cutoff, balances) {
      if (!tenantId.trim()) throw new Error("tenantId is required");
      const existing = snapshots.get(key(tenantId, cutoff));
      if (existing) return existing;
      const snapshot = createBalanceSnapshot(tenantId, cutoff, balances);
      snapshots.set(key(tenantId, cutoff), snapshot);
      return snapshot;
    },
    get(tenantId, cutoff) {
      return snapshots.get(key(tenantId, cutoff));
    },
  };
}
