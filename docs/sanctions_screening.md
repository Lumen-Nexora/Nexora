# Sanctions screening: OFAC, UN and EU (issue #123)

The daily compliance:sanctions_refresh job downloads the US OFAC SDN XML,
UN Security Council consolidated XML, and European Commission Financial
Sanctions File (FSF) 1.1 XML. Each source is parsed independently, stored
under its own source key and audited separately.

## Configuration

- OFAC_SDN_URL: official US Treasury SDN XML feed
- UN_SANCTIONS_URL: official UN Security Council consolidated XML
- EU_SANCTIONS_URL: official EU Commission FSF 1.1 full XML export. The
  Commission export can require an account-specific token parameter. An
  HTTP 403/401 or non-XML response is an explicit refresh failure, not an
  empty sanctions list. Obtain an access token through the official FSF
  portal, configure it securely, and never commit it or log the full URL.
- COMPLIANCE_SIMILARITY_THRESHOLD: minimum combined Jaro-Winkler and Double
  Metaphone score, default 0.85. Invalid values fall back to default.
- COMPLIANCE_RELOAD_MINUTES: API/worker reload interval, default 15 minutes.

The EU URL points at the official source, not a substitute mirror.
Deployment needs working EU export authentication when gated.
The UN endpoint can redirect to a signed hosting service.

## Safety and lifecycle

- All three publishers must be present in persisted data before a screener
  is ready. Otherwise, the screener errors and the composite holds transfers
  for manual review.
- Failed/empty/partial refreshes do not replace existing valid source data;
  other sources refresh independently. Failures produce an audit and webhook.
- A possible name match only holds for manual review; exact sanctioned
  digital-currency address matches block. The hold reason includes score,
  source ID, Jaro-Winkler and phonetic rationale.
- The candidate index is immutable between refreshes. Numeric business
  identifiers get dedicated postings to avoid scanning all shared names.
- Source UIDs are namespaced to avoid collisions across publishers.

## Verification

    go test ./internal/compliance ./internal/domain ./internal/transfer ./internal/config
    go test -race ./internal/compliance
    go test ./internal/compliance -run '^$' -bench 'BenchmarkSanctionsScreener' -benchtime=300ms

Fixtures exercise source IDs and aliases, partial-source fail-closed status,
malformed or empty XML, independent refreshes, persistence across errors,
a configurable threshold, and positive and negative matching.
Benchmarks are synthetic and should be repeated on representative datasets.
