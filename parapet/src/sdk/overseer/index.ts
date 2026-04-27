// Overseer SDK shim — re-exports the overseer-domain types from generated
// protobuf so that callers do not depend directly on gen paths.
//
// Castle bindings (castle_connect, castle_pb) are NOT re-exported here;
// those are overlord-internal and stay as direct gen imports.
//
// adapter_plugin_pb is intentionally omitted: no Parapet consumer currently
// uses it. Add named exports here when a consumer is introduced.

export { OverseerService } from '../../gen/overlord/v1/overseer_connect';
export type { Run } from '../../gen/overlord/v1/overseer_pb';
export { Envelope, LogStream, StepLog, WatchReady } from '../../gen/overlord/v1/events_pb';
