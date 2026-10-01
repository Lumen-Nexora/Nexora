export interface WalletBalance { tenantId: string; wallet: string; asset: string; available: string; pending: string; sourceLedger: number; stale: boolean; }
export interface BalanceSnapshot { tenantId: string; cutoff: string; rows: WalletBalance[]; stale: boolean; }

export function createBalanceSnapshot(tenantId: string, cutoff: Date, balances: readonly WalletBalance[]): BalanceSnapshot {
  if (!tenantId.trim() || Number.isNaN(cutoff.getTime())) throw new Error("tenantId and valid cutoff are required");
  const rows = balances.filter((row) => row.tenantId === tenantId).map((row) => ({ ...row })).sort((a, b) => (a.wallet + "|" + a.asset).localeCompare(b.wallet + "|" + b.asset));
  return { tenantId, cutoff: cutoff.toISOString(), rows, stale: rows.some((row) => row.stale) };
}
const csv = (value: string) => /[",\n]/.test(value) ? '"' + value.replaceAll('"', '""') + '"' : value;
export function snapshotCsv(snapshot: BalanceSnapshot): string {
  const header = "wallet,asset,available,pending,sourceLedger,stale";
  const rows = snapshot.rows.map((row) => [row.wallet,row.asset,row.available,row.pending,String(row.sourceLedger),String(row.stale)].map(csv).join(","));
  return [header, ...rows].join("\n") + "\n";
}
