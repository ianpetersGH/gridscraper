# NYSEG safety and historical CSV import

## What the current scraper actually does

`fetch nyseg` submits one browser login, opens the NYSEG insights export dialog,
requests CSV, and polls the export job. The `v2/usage` endpoint is a billing-summary
endpoint, **not an hourly source**. The legacy `NYSEGDirectScraper` Go type name and
upstream module import paths remain for compatibility.

The current dialog automation exports the **portal-selected range**. Merely
calculating `days_to_fetch` did not set that range upstream. This fork reports that
limitation instead of claiming a lookback it did not request. It does not discard
rows to pretend a range was applied. No live login/export has been verified without
real credentials; portal selectors and CSV schema remain subject to live validation.
Cookies alone do not support this active fetch flow. `fetch nyseg` does not call
`RefreshAuth`, retry a failed credential submission, or save/rewrite the config
(including JSON config files accepted as YAML). Offline import needs no config.

## Historical import: explicit normalized format only

```sh
gridscraper --db /path/to/new-gridscraper.db import-csv nyseg /path/to/normalized.csv
```

The file must contain exactly these three columns, in this order:

```csv
start_time,end_time,kwh
2026-11-01T01:00:00-04:00,2026-11-01T01:00:00-05:00,0
2026-11-01T01:00:00-05:00,2026-11-01T02:00:00-05:00,1.25
```

This is a **documented interchange schema**, not an assumed Google Sheet layout or
an automatic mapping of any user's spreadsheet. Convert a reviewed source explicitly.
Do not synthesize offsets for ambiguous source hours. The example is synthetic and
shows both occurrences of the autumn repeated hour.

Rules:

- Timestamps require full RFC3339 dates/times with `Z` or a numeric offset. Fractional
  seconds up to nanosecond precision are accepted. End must be strictly after start.
- `kwh` is a plain finite nonnegative decimal, in **kWh**; zero is valid. No suffix,
  thousands separators, scientific notation, negative sign, NaN, infinity, or blank
  values in the normalized format. Export/net-negative readings are unsupported;
  they require a separate model, not a sign flip or dropped row.
- Duplicate or overlapping intervals within a file are errors. Gaps and unsorted,
  nonoverlapping intervals are permitted. It imports intervals as given; it does not
  invent missing hours or resample arbitrary intervals into hourly records.
- The entire file is validated **before the DB is opened**. Inserts are transactional.
  Re-importing identical stored intervals is idempotent. Different kWh/end times for
  an existing start, overlaps with existing history, or an SQL failure roll back the
  whole batch. Counts report actual inserts and unchanged records.
- Import only writes the selected GridScraper SQLite database. It does not log in,
  access Home Assistant, publish data, or load/save credentials.

## Portal CSV validation

The browser export parser is separate from the normalized import parser. It accepts
`Date`, `Start Time`, `End Time` plus `Usage`/`Net Units`/`Net`, with explicit kWh units
in a supported header (e.g. `Usage (kWh)`) or a `Unit`/`Units` column. Bare usage
columns with no declared units are rejected. Additional non-required portal columns
are ignored; multiple recognized required columns are rejected. Unsupported exports
must be explicitly normalized, not guessed.

Portal values may have a kWh suffix and correctly grouped thousands separators.
Every row must parse; errors do not silently skip rows. Timezone-less portal times
are interpreted in `America/New_York`, never the machine timezone. Ambiguous autumn
hours and nonexistent spring hours are rejected with instructions to supply offsets.
Date plus clock-only times and end-of-day `00:00`/`24:00` are supported. Zeros are not
filtered out and hourly records are not accidentally summed into daily totals.

## UTC storage and legacy databases

New NYSEG interval start/end values are stored as canonical UTC RFC3339Nano strings with
`Z` (whole-second values use ordinary RFC3339), not naive UTC wall-clock text. The
unique `(start_time, service)` key therefore distinguishes both DST fallback hours
while deduplicating identical instants represented by different source offsets.
The separate `date` field remains the NYSEG local calendar date for grouping.

Legacy NYSEG rows without trustworthy offsets are **not automatically migrated**.
Reads/inserts fail rather than interpreting them in the host timezone or guessing
which repeated hour they represented. Use a new GridScraper DB and reimport reviewed,
offset-qualified source history. Keep the old file unchanged for audit/recovery.
This work does not open or migrate any user's actual database.

## Browser and log policy

- No auth-header values, response bodies, signed CSV URLs, promise identifiers,
  or post-login redirect URLs are printed by the NYSEG scraper. Arbitrary Chrome/
  HTTP error text is suppressed where it could contain URLs or script data.
- Raw `capture nyseg`, `debug nyseg` and `inspect nyseg` diagnostics are disabled
  before reading config: unredacted HTML/network dumps can contain credentials.
  `fetch nyseg --visible` uses an ephemeral profile, not a fixed shared profile or
  a fixed externally attachable debug port. Do not publish manual browser dumps.
- **Virtual display:** set `GRIDSCRAPER_VIRTUAL_DISPLAY=1` only when a local X
  display is supplied (for example, `xvfb-run -a gridscraper fetch nyseg`). This
  runs ordinary Chromium rather than headless Chromium without exposing a GUI
  port. A credential-free A/B test on the same host and container loaded the
  canonical `https://sso.nyseg.com/` form in normal mode while headless mode
  returned `net::ERR_HTTP2_PROTOCOL_ERROR`. The default remains headless unless
  `--visible` or this explicit environment option is selected. This changes
  browser mode, not TLS validation, site isolation, or sandbox policy.
- Site isolation and the Chromium sandbox remain enabled by default. The policy
  also overrides chromedp defaults that disable site-per-process and protection
  features. Running as root is not supported as a sandbox bypass.
- **Container limitation:** where the container runtime/kernel cannot support the
  Chromium sandbox, an operator may explicitly set `GRIDSCRAPER_NO_SANDBOX=1` for a
  **non-root** process. This reduces browser isolation; a non-root UID is not a
  replacement sandbox. Use a dedicated, minimally privileged container with no
  Docker socket, no broad home mounts, only necessary config/data access, dropped
  capabilities, and a read-only root filesystem where practical. Never assume that
  `--no-sandbox` is safe simply because the process is in Docker.
- Auth capture trusts only the expected HTTPS API host. HTTP export polling does
  not follow redirects or forward custom auth headers to signed storage URLs.
  Only HTTPS S3 download endpoints are accepted; a future portal storage-host
  change fails closed. Partial exports are not imported. Response size is bounded.

Con Edison is outside this NYSEG security audit; do not assume its upstream debug
logging or authentication flow has received the same hardening. The inherited
AppDaemon helper directly modifies Recorder tables: **do not deploy it**. Alternative
Home Assistant integration must use a supported, separately reviewed path.

## Offline validation

`./scripts/test-docker.sh` tests, vets and builds from a source-only snapshot using
a digest-pinned Go image and an unprivileged UID. It never mounts the user's home,
credentials or a Docker socket into the test container. Build/cache output is under
ignored `.build/`. Tests use synthetic CSV and temporary DBs; the browser-policy
test uses a fake executable, not a real login. Live authenticated fetching is not
covered by these offline checks.
