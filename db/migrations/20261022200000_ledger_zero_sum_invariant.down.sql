DROP TRIGGER IF EXISTS trg_journal_entries_immutable ON journal_entries;
DROP TRIGGER IF EXISTS trg_ledger_postings_immutable ON ledger_postings;
DROP FUNCTION IF EXISTS ledger_prevent_mutation();
DROP FUNCTION IF EXISTS ledger_enforce_zero_sum();
DROP TABLE IF EXISTS ledger_postings;
DROP TABLE IF EXISTS journal_entries;
DROP TABLE IF EXISTS ledger_accounts;
DROP TABLE IF EXISTS sweep_halts;
DROP TABLE IF EXISTS pooled_reserve_wallets;
