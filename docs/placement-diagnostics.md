# Explain a refused placement

Each informational `launch result` log keeps `instance`, `tenant`, `job`,
`launch_id`, `service`, and `status` together with the context for that attempt.
Select all six correlation fields when investigating repeated `no_capacity`.
`service` and `status` describe the final placement result; `attempts` preserves
providers considered earlier in fallback order.

`requested` describes the acquired lease's effective base selector, platform
constraint, capabilities, CPU millicores, memory bytes, and scratch bytes after
Pulse defaults and leased overrides. Each fact has a `state` and `value`:

- `known` records the value used for placement. An empty capability list is `[]`.
- `unspecified` with `null` means there was no resource constraint.
- `unknown` with `null` and an explanation means the fact is unavailable. For
  example, a wildcard base does not establish a particular platform. Failed
  preparation may leave the allocation unknown.

`preparation` records the stage and original returned error before submission.
`attempts` names each considered configured service and provider type, preserves
original capability discovery and submission errors, separates validation errors,
and keeps matching original provider status/Reason values before normalization.
A compatibility skip is a controller decision with `submit_called=false`, not a
provider capacity refusal. Unvisited providers have no attempt entry. Errors
from a batch call have `scope=batch`; an error accompanying a valid per-item
result does not override that result. The macOS shim additionally preserves
original per-launch runtime results/errors before its own normalization.

Remote HTTP failures retain their returned error bodies, including partial reads,
within the existing 16 MiB response limit, with explicit truncation when exceeded.
The cloud HTTP adapters use the same response bound and retain credential
preparation causes. Plugin submission method errors travel separately from
per-item results, preserving original text and existing error classification.
The plugin SDK captures newly transported error text within the negotiated
response bound, prioritizing existing results and explicitly marking truncation
with the retained byte count when exceeded. The controller retains the returned
text in full, including that marker.
HTTP and plugin transports also preserve matching original status/Reason values
before validating admission results, including duplicates and malformed Reasons.
These operator observations never become authoritative admission results.
Plugin observations use bytes on the wire, separate from validated per-item results.
They share the negotiated response budget after results and method errors;
`original_results_error` explicitly reports partial capture or an older SDK that
cannot supply observations before normalization. Unrelated launch IDs are excluded.
Upgrade plugins built with an older SDK to expose errors their server discarded.
RPC transport failures retain their concrete cause while remaining uncertain.

Each attempt's `provider_preparation` preserves the original error returned by
the controller's existing provider maintenance call. Its scope is `provider`,
not an individual launch. `error_returned` retains the raw string;
`no_error_returned` describes that call's return without proving preparation or
provider health. `unknown` means no maintenance diagnostic was available. This
keeps early preparation failures correlated even when the runtime could not yet
attach their cause to a per-launch Reason.

Pulse logs the captured returned errors and original Reasons without further
truncation or replacement; the finite transport capture bounds are described above. JSON/text escaping preserves multiline and quoted text. The
record contains no full launch, environment or exported lease. Provider errors
stay in operator logs; public HTTP responses retain their existing shapes.

VZ reports reached admission gates and native causes in its Reason and error
channels. Pulse logs the text returned by the runtime. A preparation error can
explain a later refusal. The provider retains that error in memory until
recovery, a relevant state change, or closure.

Capacity observations are explicitly unknown when the provider contract does
not supply them. Read the original Reason for an observed admission gate, such
as `macOS slots are being reconciled`. Capability declarations, instance counts,
paused VMs, empty health items, and generic capacity configuration do not measure
free slots, available workers, or the constraint that rejected this placement.

`execution_admission=not_accepted` makes a definite refusal a placement outcome
before accepted execution. Ordinary `no_capacity` is informational backoff, not a
recipe failure. `accepted` means the provider accepted the launch, without
asserting recipe execution. `uncertain` retains ownership because execution may
have started. Accepted and uncertain outcomes receive no retry deadline from
this attempt.

For a definite refusal, `retry` reports:

- `cooldown` and `local_eligible_at`, taken from the stored scheduler entry.
- `release.outcome`, the exact requested JobDB deadline, the acknowledged deadline
  when Reschedule succeeds, and any cleanup error. Failed release leaves the
  acknowledged deadline unknown while retaining the attempted deadline.
- `next_eligible_at`, the later of the acknowledged JobDB and local deadlines.
  It is unknown when JobDB release did not acknowledge a deadline.

Deadlines are UTC with available precision. Eligibility is a lower bound, not a
promise of dispatch at that instant: polling, lease arbitration, and provider
admission still apply. The default 60-second cooldown and 5-second polling can
therefore yield attempts roughly 65 seconds apart. `AWAITING_FUTURE` following a
refusal can describe this backoff rather than a task suspended inside a VM.

[HTTP status](http-api.md#status) describes controller polling. Successful passes
can coexist with repeated placement refusals. Investigate the correlated launch
record to explain placement; instance listings alone cannot recover a historical
admission reason that was never returned.
